package processors

import (
	"context"
	"fmt"
	"time"

	"github.com/stellar/go-stellar-sdk/support/log"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/stellar/wallet-backend/internal/indexer/types"
	"github.com/stellar/wallet-backend/internal/metrics"
)

// Bounds on the walk of one operation's declared auth invocation trees. The trees are
// submitter input: without these the walk is as deep and as wide as whatever the XDR
// decoder, core and the host happen to accept. Past a bound the walk stops descending
// and the operation keeps what was found so far. It must never return an error for
// this: core has already included the transaction, so an error here would fail the
// same ledger on every retry.
//
// Both bounds sit above what a transaction in a closed ledger carries today, so they
// drop no real deploy. The deepest auth tree stellar-core v30.0.0 applies is 245
// invocations in one chain (246 is included but fails): the host decodes each auth
// entry with an XDR depth limit of 500, two levels per nested invocation. A
// transaction of the maximum size (132,096 bytes) holds fewer than 2,600 invocations.
const (
	maxAuthWalkDepth = 256
	maxAuthWalkNodes = 4096
)

// ContractDeployProcessor emits state changes for contract deployments.
type ContractDeployProcessor struct {
	networkPassphrase string
	metricsService    *metrics.IngestionMetrics
}

func NewContractDeployProcessor(networkPassphrase string, metricsService *metrics.IngestionMetrics) *ContractDeployProcessor {
	return &ContractDeployProcessor{
		networkPassphrase: networkPassphrase,
		metricsService:    metricsService,
	}
}

func (p *ContractDeployProcessor) Name() string {
	return "contract_deploy"
}

func (p *ContractDeployProcessor) StateChangeSubBase() int64 {
	return types.StateChangeSubBaseContractDeploy
}

// ProcessOperation emits a state change for each contract deployment (including subinvocations).
func (p *ContractDeployProcessor) ProcessOperation(ctx context.Context, op *TransactionOperationWrapper) ([]types.StateChange, error) {
	if op.OperationType() != xdr.OperationTypeInvokeHostFunction {
		return nil, ErrInvalidOpType
	}

	startTime := time.Now()
	defer func() {
		if p.metricsService != nil {
			duration := time.Since(startTime).Seconds()
			p.metricsService.StateChangeProcessingDuration.WithLabelValues("ContractDeployProcessor").Observe(duration)
		}
	}()

	// A failed transaction deploys nothing, whatever its host function declares.
	if !op.Transaction.Successful() {
		return nil, nil
	}
	invokeHostOp := op.Operation.Body.MustInvokeHostFunctionOp()

	opID := op.ID()
	builder := NewStateChangeBuilder(op.Transaction.Ledger.LedgerSequence(), op.LedgerClosed.Unix(), op.TransactionID()).
		WithOperationID(opID).
		WithCategory(types.StateChangeCategoryAccount).
		WithReason(types.StateChangeReasonCreate)

	var stateChanges []types.StateChange
	// seen dedupes contract IDs across the host function and its subinvocations;
	// most InvokeHostFunction operations deploy nothing and never populate it.
	var seen map[string]struct{}

	emitCreate := func(contractID string, fromAddr xdr.ContractIdPreimageFromAddress) error {
		// The host rejects a deployer address it can't convert to a host object, so a deploy
		// from one never creates a contract and there is nothing to record.
		deployerAddr, storable, err := deployerAddressString(fromAddr.Address)
		if err != nil {
			return fmt.Errorf("deployer address to string: %w", err)
		}
		if !storable {
			return nil
		}
		if _, ok := seen[contractID]; ok {
			return nil
		}
		if seen == nil {
			seen = make(map[string]struct{})
		}
		seen[contractID] = struct{}{}
		stateChanges = append(stateChanges, builder.WithAccount(contractID).WithCreator(deployerAddr).Build())
		return nil
	}

	// The top-level host function is authenticated: the host calls require_auth on the
	// FromAddress, so this (successful) operation proves the deploy and its deployer.
	hf := invokeHostOp.HostFunction
	switch hf.Type {
	case xdr.HostFunctionTypeHostFunctionTypeCreateContract:
		if err := p.emitTopLevel(hf.MustCreateContract().ContractIdPreimage, emitCreate); err != nil {
			return nil, err
		}
	case xdr.HostFunctionTypeHostFunctionTypeCreateContractV2:
		if err := p.emitTopLevel(hf.MustCreateContractV2().ContractIdPreimage, emitCreate); err != nil {
			return nil, err
		}
	case xdr.HostFunctionTypeHostFunctionTypeUploadContractWasm, xdr.HostFunctionTypeHostFunctionTypeInvokeContract:
		// no-op
	}

	if len(invokeHostOp.Auth) == 0 {
		return stateChanges, nil
	}

	// Nested deploys are declared in the auth tree, which the submitter controls and the
	// host does not check for unmatched entries — a declared
	// CreateContract is recorded only when the meta holds the created instance entry,
	// which proves the deploy ran; the declaration then supplies the deployer address.
	created, err := createdContractInstances(op)
	if err != nil {
		return nil, err
	}
	// No instance was created, so no declaration can be confirmed: skip the walk.
	if len(created) == 0 {
		return stateChanges, nil
	}
	// visited counts invocations across every auth entry of the operation; truncatedBy
	// names the first bound that cut the walk short ("depth" or "nodes").
	visited := 0
	truncatedBy := ""
	var walkInvocation func(inv xdr.SorobanAuthorizedInvocation, depth int) error
	walkInvocation = func(inv xdr.SorobanAuthorizedInvocation, depth int) error {
		if depth >= maxAuthWalkDepth {
			if truncatedBy == "" {
				truncatedBy = "depth"
			}
			return nil
		}
		visited++
		// Bind the args to a local first: a field of a function's return value is not
		// addressable.
		var preimage *xdr.ContractIdPreimage
		switch inv.Function.Type {
		case xdr.SorobanAuthorizedFunctionTypeSorobanAuthorizedFunctionTypeCreateContractHostFn:
			cc := inv.Function.MustCreateContractHostFn()
			preimage = &cc.ContractIdPreimage
		case xdr.SorobanAuthorizedFunctionTypeSorobanAuthorizedFunctionTypeCreateContractV2HostFn:
			cc := inv.Function.MustCreateContractV2HostFn()
			preimage = &cc.ContractIdPreimage
		case xdr.SorobanAuthorizedFunctionTypeSorobanAuthorizedFunctionTypeContractFn:
			// no-op
		}
		if preimage != nil && preimage.Type == xdr.ContractIdPreimageTypeContractIdPreimageFromAddress {
			fromAddr := preimage.MustFromAddress()
			contractID, err := calculateContractID(p.networkPassphrase, fromAddr)
			if err != nil {
				return fmt.Errorf("calculating contract ID: %w", err)
			}
			if _, ok := created[contractID]; ok {
				if err := emitCreate(contractID, fromAddr); err != nil {
					return err
				}
			}
		}
		for _, sub := range inv.SubInvocations {
			if visited >= maxAuthWalkNodes {
				if truncatedBy == "" {
					truncatedBy = "nodes"
				}
				break
			}
			if err := walkInvocation(sub, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	for _, auth := range invokeHostOp.Auth {
		if visited >= maxAuthWalkNodes {
			if truncatedBy == "" {
				truncatedBy = "nodes"
			}
			break
		}
		if err := walkInvocation(auth.RootInvocation, 0); err != nil {
			return nil, err
		}
	}
	if truncatedBy != "" {
		// Count and log it rather than fail: the ledger is valid, only this walk is cut short.
		if p.metricsService != nil {
			p.metricsService.AuthTreeWalkTruncationsTotal.WithLabelValues(truncatedBy).Inc()
		}
		log.Ctx(ctx).Warnf(
			"processor: %s: auth tree walk stopped at the %s bound (max depth %d, max nodes %d); nested deploys declared past it are not recorded: txHash: %s, opID: %d",
			p.Name(), truncatedBy, maxAuthWalkDepth, maxAuthWalkNodes, op.Transaction.Result.TransactionHash.HexString(), opID,
		)
	}

	return stateChanges, nil
}

// emitTopLevel records the operation's own CreateContract when its preimage names a
// deployer address. FromAsset preimages deploy a SAC, which has no creator account.
func (p *ContractDeployProcessor) emitTopLevel(preimage xdr.ContractIdPreimage, emit func(contractID string, fromAddr xdr.ContractIdPreimageFromAddress) error) error {
	if preimage.Type != xdr.ContractIdPreimageTypeContractIdPreimageFromAddress {
		return nil
	}
	fromAddr := preimage.MustFromAddress()
	contractID, err := calculateContractID(p.networkPassphrase, fromAddr)
	if err != nil {
		return fmt.Errorf("calculating contract ID: %w", err)
	}
	return emit(contractID, fromAddr)
}

// createdContractInstances returns the C-addresses whose contract instance entry this
// operation created, per the ledger entry changes in its meta.
func createdContractInstances(op *TransactionOperationWrapper) (map[string]struct{}, error) {
	changes, err := op.Changes()
	if err != nil {
		return nil, fmt.Errorf("getting operation changes: %w", err)
	}
	created := map[string]struct{}{}
	for _, change := range changes {
		// ChangeType, not Pre == nil: a protocol-23 hot-archive restore also surfaces
		// with no Pre, and a restored instance is not a deploy.
		if change.Type != xdr.LedgerEntryTypeContractData || change.ChangeType != xdr.LedgerEntryChangeTypeLedgerEntryCreated || change.Post == nil {
			continue
		}
		contractData := change.Post.Data.MustContractData()
		if contractData.Key.Type != xdr.ScValTypeScvLedgerKeyContractInstance {
			continue
		}
		contractID, err := contractData.Contract.String()
		if err != nil {
			return nil, fmt.Errorf("converting contract address to string: %w", err)
		}
		created[contractID] = struct{}{}
	}
	return created, nil
}
