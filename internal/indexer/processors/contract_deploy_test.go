package processors

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/xdr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stellar/wallet-backend/internal/indexer/types"
	"github.com/stellar/wallet-backend/internal/metrics"
	"github.com/stellar/wallet-backend/internal/utils"
)

func Test_ContractDeployProcessor_Process_invalidOpType(t *testing.T) {
	ctx := context.Background()
	proc := NewContractDeployProcessor(network.TestNetworkPassphrase, nil)

	op := &TransactionOperationWrapper{
		Operation: xdr.Operation{Body: xdr.OperationBody{Type: xdr.OperationTypePayment}},
	}
	changes, err := proc.ProcessOperation(ctx, op)
	assert.ErrorIs(t, err, ErrInvalidOpType)
	assert.Nil(t, changes)
}

func Test_ContractDeployProcessor_Process_createContract(t *testing.T) {
	const (
		opSourceAccount   = "GBZURSTQQRSU3XB66CHJ3SH2ZWLG663V5SWM6HF3FL72BOMYHDT4QTUF"
		fromSourceAccount = "GCQIH6MRLCJREVE76LVTKKEZXRIT6KSX7KU65HPDDBYFKFYHIYSJE57R"
		authSignerAccount = "GDG2KKXC62BINMUZNBTLG235323N6BOIR33JBF4ELTOUKUG5BDE6HJZT"
	)

	ctx := context.Background()

	builder := NewStateChangeBuilder(12345, closeTime.Unix(), 53021371269120).
		WithOperationID(53021371269121).
		WithReason(types.StateChangeReasonCreate).
		WithCategory(types.StateChangeCategoryAccount)

	type TestCase struct {
		name             string
		op               *TransactionOperationWrapper
		wantStateChanges []types.StateChange
	}
	testCases := []TestCase{}

	// We test with/without subinvocations, which can contain nested invocations to this or another contract.
	for _, withSubinvocations := range []bool{false, true} {
		// We test with/without fee bump, to ensure the processor handles both cases.
		for _, feeBump := range []bool{false, true} {
			for _, hostFnType := range []xdr.HostFunctionType{xdr.HostFunctionTypeHostFunctionTypeCreateContract, xdr.HostFunctionTypeHostFunctionTypeCreateContractV2} {
				prefix := strings.ReplaceAll(hostFnType.String(), "HostFunctionTypeHostFunctionType", "")

				subInvocationsStateChanges := []types.StateChange{}
				if withSubinvocations {
					prefix = fmt.Sprintf("%s,withSubinvocations🔄", prefix)
					subInvocationsStateChanges = []types.StateChange{
						builder.
							WithCreator(deployerAccountID).
							WithAccount(deployedContractID).
							Build(),
					}
				}

				if feeBump {
					prefix = fmt.Sprintf("feeBump(%s)", prefix)
				}

				testCases = append(testCases,
					TestCase{
						name: fmt.Sprintf("🟢%s/FromAddress/tx.SourceAccount", prefix),
						op: func() *TransactionOperationWrapper {
							op := makeBasicSorobanOp()
							setFromAddress(op, hostFnType, fromSourceAccount)
							if withSubinvocations {
								op.Operation.Body.InvokeHostFunctionOp.Auth = makeAuthEntries(t, op, makeScAddress(authSignerAccount))
								includeSubInvocations(op)
								setOperationMeta(op, xdr.LedgerEntryChanges{contractInstanceCreated(deployedContractID)}, nil)
							}
							if feeBump {
								op = makeFeeBumpOp(txSourceAccount, op)
							}
							return op
						}(),
						wantStateChanges: append(subInvocationsStateChanges,
							builder.
								WithCreator(fromSourceAccount).
								WithAccount("CA7UGIYR2H63C2ETN2VE4WDQ6YX5XNEWNWC2DP7A64B2ZR7VJJWF3SBF").
								Build(),
						),
					},
					TestCase{
						name: fmt.Sprintf("🟢%s/FromAddress/op.SourceAccount", prefix),
						op: func() *TransactionOperationWrapper {
							op := makeBasicSorobanOp()
							op.Operation.SourceAccount = utils.PointOf(xdr.MustMuxedAddress(opSourceAccount))
							setFromAddress(op, hostFnType, fromSourceAccount)
							if withSubinvocations {
								op.Operation.Body.InvokeHostFunctionOp.Auth = makeAuthEntries(t, op, makeScAddress(authSignerAccount))
								includeSubInvocations(op)
								setOperationMeta(op, xdr.LedgerEntryChanges{contractInstanceCreated(deployedContractID)}, nil)
							}
							if feeBump {
								op = makeFeeBumpOp(txSourceAccount, op)
							}
							return op
						}(),
						wantStateChanges: append(subInvocationsStateChanges,
							builder.
								WithCreator(fromSourceAccount).
								WithAccount("CA7UGIYR2H63C2ETN2VE4WDQ6YX5XNEWNWC2DP7A64B2ZR7VJJWF3SBF").
								Build(),
						),
					},
					TestCase{
						name: fmt.Sprintf("🟢%s/FromAsset/tx.SourceAccount", prefix),
						op: func() *TransactionOperationWrapper {
							op := makeBasicSorobanOp()
							setFromAsset(op, hostFnType, usdcAssetTestnet)
							if withSubinvocations {
								op.Operation.Body.InvokeHostFunctionOp.Auth = makeAuthEntries(t, op, makeScAddress(authSignerAccount))
								includeSubInvocations(op)
								setOperationMeta(op, xdr.LedgerEntryChanges{contractInstanceCreated(deployedContractID)}, nil)
							}
							if feeBump {
								op = makeFeeBumpOp(txSourceAccount, op)
							}
							return op
						}(),
						wantStateChanges: subInvocationsStateChanges,
					},
				)
			}
		}
	}

	proc := NewContractDeployProcessor(network.TestNetworkPassphrase, nil)
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			stateChanges, err := proc.ProcessOperation(ctx, tc.op)

			require.NoError(t, err)
			assertStateChangesElementsMatch(t, tc.wantStateChanges, stateChanges)
		})
	}
}

// Test_ContractDeployProcessor_Process_multipleContractsDeterministicOrder pins the emission
// order for an operation that deploys several contracts across its root host function and its
// auth invocation tree. AssignStateChangeOrdinals (internal/indexer/types/types.go) derives each
// state change's ID from its slice position within the operation, so re-ingesting the same ledger
// must yield the same slice order every time. This test asserts the exact order (not
// ElementsMatch) and also exercises dedup: the auth tree redeclares the same contract creation
// that a sibling subinvocation already produced, and it must not be emitted twice.
func Test_ContractDeployProcessor_Process_multipleContractsDeterministicOrder(t *testing.T) {
	const (
		rootDeployer = "GCQIH6MRLCJREVE76LVTKKEZXRIT6KSX7KU65HPDDBYFKFYHIYSJE57R"
		subDeployerB = "GDG2KKXC62BINMUZNBTLG235323N6BOIR33JBF4ELTOUKUG5BDE6HJZT"
		subDeployerC = "GAGWN4445WLODCXT7RUZXJLQK5XWX4GICXDOAAZZGK2N3BR67RIIVWJ7"

		contractA = "CA7UGIYR2H63C2ETN2VE4WDQ6YX5XNEWNWC2DP7A64B2ZR7VJJWF3SBF" // rootDeployer + TestSalt, root host function
	)
	saltB := xdr.Uint256{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1}
	saltC := xdr.Uint256{2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2}

	preimageB := xdr.ContractIdPreimageFromAddress{Address: makeScAddress(subDeployerB), Salt: saltB}
	preimageC := xdr.ContractIdPreimageFromAddress{Address: makeScAddress(subDeployerC), Salt: saltC}

	contractB, err := calculateContractID(network.TestNetworkPassphrase, preimageB)
	require.NoError(t, err)
	contractC, err := calculateContractID(network.TestNetworkPassphrase, preimageC)
	require.NoError(t, err)

	ctx := context.Background()

	op := makeBasicSorobanOp()
	setFromAddress(op, xdr.HostFunctionTypeHostFunctionTypeCreateContract, rootDeployer)
	op.Operation.Body.InvokeHostFunctionOp.Auth = []xdr.SorobanAuthorizationEntry{
		{
			RootInvocation: xdr.SorobanAuthorizedInvocation{
				Function: xdr.SorobanAuthorizedFunction{
					Type: xdr.SorobanAuthorizedFunctionTypeSorobanAuthorizedFunctionTypeContractFn,
				},
				SubInvocations: []xdr.SorobanAuthorizedInvocation{
					{
						// Deploys contractB, then walks depth-first into its own SubInvocations
						// (deploying contractC) before the loop moves on to the next sibling.
						Function: xdr.SorobanAuthorizedFunction{
							Type:                 xdr.SorobanAuthorizedFunctionTypeSorobanAuthorizedFunctionTypeCreateContractHostFn,
							CreateContractHostFn: &xdr.CreateContractArgs{ContractIdPreimage: xdr.ContractIdPreimage{Type: xdr.ContractIdPreimageTypeContractIdPreimageFromAddress, FromAddress: &preimageB}},
						},
						SubInvocations: []xdr.SorobanAuthorizedInvocation{
							{
								Function: xdr.SorobanAuthorizedFunction{
									Type:                   xdr.SorobanAuthorizedFunctionTypeSorobanAuthorizedFunctionTypeCreateContractV2HostFn,
									CreateContractV2HostFn: &xdr.CreateContractArgsV2{ContractIdPreimage: xdr.ContractIdPreimage{Type: xdr.ContractIdPreimageTypeContractIdPreimageFromAddress, FromAddress: &preimageC}},
								},
							},
						},
					},
					{
						// Redeclares the same contractB creation as the sibling above; must be
						// deduped and not appear a second time in the output.
						Function: xdr.SorobanAuthorizedFunction{
							Type:                 xdr.SorobanAuthorizedFunctionTypeSorobanAuthorizedFunctionTypeCreateContractHostFn,
							CreateContractHostFn: &xdr.CreateContractArgs{ContractIdPreimage: xdr.ContractIdPreimage{Type: xdr.ContractIdPreimageTypeContractIdPreimageFromAddress, FromAddress: &preimageB}},
						},
					},
				},
			},
		},
	}

	setOperationMeta(op, xdr.LedgerEntryChanges{contractInstanceCreated(contractB), contractInstanceCreated(contractC)}, nil)

	proc := NewContractDeployProcessor(network.TestNetworkPassphrase, nil)
	stateChanges, err := proc.ProcessOperation(ctx, op)
	require.NoError(t, err)

	builder := NewStateChangeBuilder(12345, closeTime.Unix(), 53021371269120).
		WithOperationID(53021371269121).
		WithReason(types.StateChangeReasonCreate).
		WithCategory(types.StateChangeCategoryAccount)

	wantOrder := []types.StateChange{
		builder.WithCreator(rootDeployer).WithAccount(contractA).Build(),
		builder.WithCreator(subDeployerB).WithAccount(contractB).Build(),
		builder.WithCreator(subDeployerC).WithAccount(contractC).Build(),
	}

	require.Len(t, stateChanges, len(wantOrder))
	for i := range wantOrder {
		assertStateChangeEqual(t, wantOrder[i], stateChanges[i])
	}
}

func Test_ContractDeployProcessor_Process_invokeContract(t *testing.T) {
	const (
		opSourceAccount   = "GBZURSTQQRSU3XB66CHJ3SH2ZWLG663V5SWM6HF3FL72BOMYHDT4QTUF"
		argAccountID1     = "GCQIH6MRLCJREVE76LVTKKEZXRIT6KSX7KU65HPDDBYFKFYHIYSJE57R"
		argAccountID2     = "GDG2KKXC62BINMUZNBTLG235323N6BOIR33JBF4ELTOUKUG5BDE6HJZT"
		argContractID1    = "CBIELTK6YBZJU5UP2WWQEUCYKLPU6AUNZ2BQ4WWFEIE3USCIHMXQDAMA"
		argContractID2    = "CDNVQW44C3HALYNVQ4SOBXY5EWYTGVYXX6JPESOLQDABJI5FC5LTRRUE"
		authSignerAccount = "GDG2KKXC62BINMUZNBTLG235323N6BOIR33JBF4ELTOUKUG5BDE6HJZT"
		invokedContractID = "CBL6KD2LFMLAUKFFWNNXWOXFN73GAXLEA4WMJRLQ5L76DMYTM3KWQVJN"
	)

	ctx := context.Background()

	makeInvokeContractOp := func(argAddresses ...xdr.ScAddress) *TransactionOperationWrapper {
		op := makeBasicSorobanOp()
		op.Operation = xdr.Operation{
			Body: xdr.OperationBody{
				Type: xdr.OperationTypeInvokeHostFunction,
				InvokeHostFunctionOp: &xdr.InvokeHostFunctionOp{
					HostFunction: xdr.HostFunction{
						Type: xdr.HostFunctionTypeHostFunctionTypeInvokeContract,
						InvokeContract: &xdr.InvokeContractArgs{
							ContractAddress: makeScContract(invokedContractID),
							FunctionName:    xdr.ScSymbol("authorized_fn"),
							Args: func() []xdr.ScVal {
								args := make([]xdr.ScVal, len(argAddresses))
								for i, argAddress := range argAddresses {
									args[i] = xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: utils.PointOf(argAddress)}
								}
								return args
							}(),
						},
					},
					Auth: []xdr.SorobanAuthorizationEntry{},
				},
			},
		}

		return op
	}

	builder := NewStateChangeBuilder(12345, closeTime.Unix(), 53021371269120).
		WithOperationID(53021371269121).
		WithReason(types.StateChangeReasonCreate).
		WithCategory(types.StateChangeCategoryAccount)

	type TestCase struct {
		name             string
		op               *TransactionOperationWrapper
		wantStateChanges []types.StateChange
	}
	testCases := []TestCase{}

	// We test with/without subinvocations, which can contain nested invocations to this or another contract.
	for _, withSubinvocations := range []bool{false, true} {
		// We test with/without fee bump, to ensure the processor handles both cases.
		for _, feeBump := range []bool{false, true} {
			prefix := ""

			subInvocationsStateChanges := []types.StateChange{}
			if withSubinvocations {
				prefix = "🔄WithSubinvocations🔄"
				subInvocationsStateChanges = []types.StateChange{
					builder.
						WithCreator(deployerAccountID).
						WithAccount(deployedContractID).
						Build(),
				}
			}

			if feeBump {
				prefix = fmt.Sprintf("feeBump(%s)", prefix)
			}

			testCases = append(testCases,
				TestCase{
					name: fmt.Sprintf("🟢%s/tx.SourceAccount", prefix),
					op: func() *TransactionOperationWrapper {
						op := makeInvokeContractOp(makeScAddress(argAccountID1), makeScAddress(argAccountID2))
						if withSubinvocations {
							op.Operation.Body.InvokeHostFunctionOp.Auth = makeAuthEntries(t, op, makeScAddress(authSignerAccount))
							includeSubInvocations(op)
							setOperationMeta(op, xdr.LedgerEntryChanges{contractInstanceCreated(deployedContractID)}, nil)
						}
						if feeBump {
							op = makeFeeBumpOp(txSourceAccount, op)
						}
						return op
					}(),
					wantStateChanges: subInvocationsStateChanges,
				},
				TestCase{
					name: fmt.Sprintf("🟢%s/op.SourceAccount", prefix),
					op: func() *TransactionOperationWrapper {
						op := makeInvokeContractOp(makeScContract(argContractID1), makeScContract(argContractID2))
						op.Operation.SourceAccount = utils.PointOf(xdr.MustMuxedAddress(opSourceAccount))
						if withSubinvocations {
							op.Operation.Body.InvokeHostFunctionOp.Auth = makeAuthEntries(t, op, makeScAddress(authSignerAccount))
							includeSubInvocations(op)
							setOperationMeta(op, xdr.LedgerEntryChanges{contractInstanceCreated(deployedContractID)}, nil)
						}
						if feeBump {
							op = makeFeeBumpOp(txSourceAccount, op)
						}
						return op
					}(),
					wantStateChanges: subInvocationsStateChanges,
				},
			)
		}
	}

	proc := NewContractDeployProcessor(network.TestNetworkPassphrase, nil)
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			stateChanges, err := proc.ProcessOperation(ctx, tc.op)

			require.NoError(t, err)
			assertStateChangesElementsMatch(t, tc.wantStateChanges, stateChanges)
		})
	}
}

// Test_ContractDeployProcessor_Process_unstorableDeployerAddress covers the CAP-0067 ScAddress
// arms that a contract-deploy preimage can carry but an account column cannot store.
//
// These reach the processor as ordinary transaction-envelope data: stellar-core's
// doCheckValidForSoroban validates isAssetValid only for the from-asset preimage and never
// inspects a from-address one, so the transaction is admitted and closed in a ledger; the host
// rejects the address only at apply time, and the indexer runs its state-change processors over
// failed transactions too. A claimable-balance address in particular would land a B... strkey in
// creator_account_id, whose 33-byte payload fails AddressBytea.Value() and wedges the ledger's
// persist transaction permanently.
func Test_ContractDeployProcessor_Process_unstorableDeployerAddress(t *testing.T) {
	ctx := context.Background()
	proc := NewContractDeployProcessor(network.TestNetworkPassphrase, nil)

	var h xdr.Hash
	for i := range h {
		h[i] = byte(i)
	}

	addrs := []struct {
		name string
		addr xdr.ScAddress
	}{
		{"claimableBalance", makeScClaimableBalance(h)},
		{"liquidityPool", makeScLiquidityPool(h)},
	}
	hostFnTypes := []xdr.HostFunctionType{
		xdr.HostFunctionTypeHostFunctionTypeCreateContract,
		xdr.HostFunctionTypeHostFunctionTypeCreateContractV2,
	}

	for _, tc := range addrs {
		for _, hostFnType := range hostFnTypes {
			prefix := strings.ReplaceAll(hostFnType.String(), "HostFunctionTypeHostFunctionType", "")
			t.Run(fmt.Sprintf("🔴%s/%s/rootHostFunction", prefix, tc.name), func(t *testing.T) {
				op := makeBasicSorobanOp()
				setFromScAddress(op, hostFnType, tc.addr)

				stateChanges, err := proc.ProcessOperation(ctx, op)
				require.NoError(t, err, "an unstorable deployer address must be skipped, not surfaced as an error that fails the ledger")
				assert.Empty(t, stateChanges, "no contract is ever created from this address, so there is nothing to record")
			})
		}
	}
}

// Test_ContractDeployProcessor_Process_unstorableDeployerAddressInAuthTree covers the same guard
// reached through the auth tree instead of the root host function. This is the wider vector: the
// processor walks invokeHostOp.Auth for every host-function type, and auth entries carry no
// signature under SOURCE_ACCOUNT credentials and are not semantically validated before apply, so
// the poisoned address can ride along on an ordinary contract invocation.
func Test_ContractDeployProcessor_Process_unstorableDeployerAddressInAuthTree(t *testing.T) {
	const goodDeployer = "GCQIH6MRLCJREVE76LVTKKEZXRIT6KSX7KU65HPDDBYFKFYHIYSJE57R"

	ctx := context.Background()
	proc := NewContractDeployProcessor(network.TestNetworkPassphrase, nil)

	var h xdr.Hash
	for i := range h {
		h[i] = byte(i)
	}
	badPreimage := xdr.ContractIdPreimageFromAddress{Address: makeScClaimableBalance(h), Salt: TestSalt}
	goodPreimage := xdr.ContractIdPreimageFromAddress{Address: makeScAddress(goodDeployer), Salt: TestSalt}

	goodContractID, err := calculateContractID(network.TestNetworkPassphrase, goodPreimage)
	require.NoError(t, err)
	badContractID, err := calculateContractID(network.TestNetworkPassphrase, badPreimage)
	require.NoError(t, err)

	createHostFn := func(preimage xdr.ContractIdPreimageFromAddress) xdr.SorobanAuthorizedFunction {
		return xdr.SorobanAuthorizedFunction{
			Type: xdr.SorobanAuthorizedFunctionTypeSorobanAuthorizedFunctionTypeCreateContractHostFn,
			CreateContractHostFn: &xdr.CreateContractArgs{
				ContractIdPreimage: xdr.ContractIdPreimage{
					Type:        xdr.ContractIdPreimageTypeContractIdPreimageFromAddress,
					FromAddress: &preimage,
				},
			},
		}
	}

	// An ordinary contract invocation — not a create-contract host function — whose auth tree
	// declares an unstorable deployment and a legitimate one as siblings under a common parent.
	// They are siblings rather than parent and child so this pins only what the guard decides:
	// whether a rejected node's own descendants should still be emitted is a separate,
	// pre-existing question about auth declarations that were never executed.
	op := makeBasicSorobanOp()
	op.Operation.Body = xdr.OperationBody{
		Type: xdr.OperationTypeInvokeHostFunction,
		InvokeHostFunctionOp: &xdr.InvokeHostFunctionOp{
			HostFunction: xdr.HostFunction{
				Type: xdr.HostFunctionTypeHostFunctionTypeInvokeContract,
				InvokeContract: &xdr.InvokeContractArgs{
					ContractAddress: makeScContract(deployedContractID),
					FunctionName:    "noop",
					Args:            []xdr.ScVal{},
				},
			},
			Auth: []xdr.SorobanAuthorizationEntry{{
				Credentials: xdr.SorobanCredentials{Type: xdr.SorobanCredentialsTypeSorobanCredentialsSourceAccount},
				RootInvocation: xdr.SorobanAuthorizedInvocation{
					Function: xdr.SorobanAuthorizedFunction{
						Type: xdr.SorobanAuthorizedFunctionTypeSorobanAuthorizedFunctionTypeContractFn,
					},
					SubInvocations: []xdr.SorobanAuthorizedInvocation{
						{Function: createHostFn(badPreimage)},
						{Function: createHostFn(goodPreimage)},
					},
				},
			}},
		},
	}

	// The meta shows both instances created, so only the guard decides which deploy is recorded.
	setOperationMeta(op, xdr.LedgerEntryChanges{contractInstanceCreated(badContractID), contractInstanceCreated(goodContractID)}, nil)

	stateChanges, err := proc.ProcessOperation(ctx, op)
	require.NoError(t, err)

	// The guard skips only the unstorable deployment; its sibling still lands.
	wantStateChanges := []types.StateChange{
		NewStateChangeBuilder(12345, closeTime.Unix(), op.TransactionID()).
			WithOperationID(op.ID()).
			WithReason(types.StateChangeReasonCreate).
			WithCategory(types.StateChangeCategoryAccount).
			WithCreator(goodDeployer).
			WithAccount(goodContractID).
			Build(),
	}
	assertStateChangesElementsMatch(t, wantStateChanges, stateChanges)

	// The property that actually closes the wedge: every creator this processor emits must
	// survive the encoder the persist path runs it through (see stateChangeCopyRow in
	// internal/data/statechanges.go). A failure here is what aborts the ledger's persist
	// transaction and halts ingestion permanently.
	for _, sc := range stateChanges {
		_, encErr := sc.CreatorAccountID.Value()
		require.NoError(t, encErr, "emitted creator_account_id %q must encode for the state_changes COPY", sc.CreatorAccountID.String())
	}
}

// Test_ContractDeployProcessor_Process_declaredOnlyDeployIsIgnored is the regression test
// for fabricated deploy records: a CreateContract declared in an unmatched auth
// entry's invocation tree produces no record unless the meta holds the created instance
// entry for that exact contract. The meta here carries an unrelated created instance, so
// an implementation that accepted any created instance would fail this test.
func Test_ContractDeployProcessor_Process_declaredOnlyDeployIsIgnored(t *testing.T) {
	const unrelatedCreatedContract = "CDNVQW44C3HALYNVQ4SOBXY5EWYTGVYXX6JPESOLQDABJI5FC5LTRRUE" // absent from the declared tree

	op := makeInvokeContractOp()
	op.Operation.Body.InvokeHostFunctionOp.Auth = []xdr.SorobanAuthorizationEntry{{
		Credentials:    xdr.SorobanCredentials{Type: xdr.SorobanCredentialsTypeSorobanCredentialsSourceAccount},
		RootInvocation: xdr.SorobanAuthorizedInvocation{Function: xdr.SorobanAuthorizedFunction{Type: xdr.SorobanAuthorizedFunctionTypeSorobanAuthorizedFunctionTypeContractFn, ContractFn: &xdr.InvokeContractArgs{ContractAddress: makeScContract(invokedContractID)}}},
	}}
	includeSubInvocations(op) // declares deployedContractID created by deployerAccountID
	// Meta: a different contract was created; deployedContractID was not.
	setOperationMeta(op, xdr.LedgerEntryChanges{contractInstanceCreated(unrelatedCreatedContract)}, nil)

	proc := NewContractDeployProcessor(network.TestNetworkPassphrase, nil)
	stateChanges, err := proc.ProcessOperation(context.Background(), op)
	require.NoError(t, err)
	assert.Empty(t, stateChanges)
}

// Test_ContractDeployProcessor_Process_failedTxDeploysNothing: a failed transaction creates
// no contract, so a CreateContract host function in it produces no record.
func Test_ContractDeployProcessor_Process_failedTxDeploysNothing(t *testing.T) {
	const fromSourceAccount = "GCQIH6MRLCJREVE76LVTKKEZXRIT6KSX7KU65HPDDBYFKFYHIYSJE57R"

	op := makeBasicSorobanOp()
	setFromAddress(op, xdr.HostFunctionTypeHostFunctionTypeCreateContract, fromSourceAccount)
	op.Transaction.Result = xdr.TransactionResultPair{Result: xdr.TransactionResult{Result: xdr.TransactionResultResult{Code: xdr.TransactionResultCodeTxFailed}}}

	proc := NewContractDeployProcessor(network.TestNetworkPassphrase, nil)
	stateChanges, err := proc.ProcessOperation(context.Background(), op)
	require.NoError(t, err)
	assert.Empty(t, stateChanges)
}

// declaredCreate is a CreateContract auth node for deployerAccountID with TestSalt, and the
// contract ID that preimage derives.
func declaredCreate(t *testing.T) (xdr.SorobanAuthorizedInvocation, string) {
	t.Helper()
	preimage := xdr.ContractIdPreimageFromAddress{Address: makeScAddress(deployerAccountID), Salt: TestSalt}
	contractID, err := calculateContractID(network.TestNetworkPassphrase, preimage)
	require.NoError(t, err)
	return xdr.SorobanAuthorizedInvocation{
		Function: xdr.SorobanAuthorizedFunction{
			Type: xdr.SorobanAuthorizedFunctionTypeSorobanAuthorizedFunctionTypeCreateContractHostFn,
			CreateContractHostFn: &xdr.CreateContractArgs{
				ContractIdPreimage: xdr.ContractIdPreimage{
					Type:        xdr.ContractIdPreimageTypeContractIdPreimageFromAddress,
					FromAddress: &preimage,
				},
				Executable: xdr.ContractExecutable{Type: xdr.ContractExecutableTypeContractExecutableWasm, WasmHash: &xdr.Hash{}},
			},
		},
	}, contractID
}

// contractFnNode is an auth node that declares a plain contract call.
func contractFnNode(subs ...xdr.SorobanAuthorizedInvocation) xdr.SorobanAuthorizedInvocation {
	return xdr.SorobanAuthorizedInvocation{
		Function: xdr.SorobanAuthorizedFunction{
			Type:       xdr.SorobanAuthorizedFunctionTypeSorobanAuthorizedFunctionTypeContractFn,
			ContractFn: &xdr.InvokeContractArgs{ContractAddress: makeScContract(invokedContractID), FunctionName: "f"},
		},
		SubInvocations: subs,
	}
}

// chainAbove wraps leaf in depth contract-call nodes, so leaf sits depth levels below the root.
func chainAbove(leaf xdr.SorobanAuthorizedInvocation, depth int) xdr.SorobanAuthorizedInvocation {
	node := leaf
	for i := 0; i < depth; i++ {
		node = contractFnNode(node)
	}
	return node
}

// opWithAuthRoots builds a successful InvokeContract operation that declares the given auth
// trees, round-trips the auth entries through XDR the way they arrive from a ledger, and
// attaches a meta in which createdContractID's instance was created.
func opWithAuthRoots(t *testing.T, createdContractID string, roots ...xdr.SorobanAuthorizedInvocation) *TransactionOperationWrapper {
	t.Helper()
	op := makeInvokeContractOp()
	for _, root := range roots {
		entry := xdr.SorobanAuthorizationEntry{
			Credentials:    xdr.SorobanCredentials{Type: xdr.SorobanCredentialsTypeSorobanCredentialsSourceAccount},
			RootInvocation: root,
		}
		raw, err := entry.MarshalBinary()
		require.NoError(t, err)
		var decoded xdr.SorobanAuthorizationEntry
		require.NoError(t, xdr.SafeUnmarshal(raw, &decoded), "the tree must be one the XDR decoder accepts")
		op.Operation.Body.InvokeHostFunctionOp.Auth = append(op.Operation.Body.InvokeHostFunctionOp.Auth, decoded)
	}
	setOperationMeta(op, xdr.LedgerEntryChanges{contractInstanceCreated(createdContractID)}, nil)
	return op
}

// Test_ContractDeployProcessor_Process_oversizedAuthTreeNeverFails pins the two properties of
// the auth-tree walk bounds. The tree is submitter input and core includes the transaction
// whatever its shape, so (1) no tree may make ProcessOperation return an error, because an
// error fails the ledger on every retry, and (2) a tree past a bound is cut short, counted
// and logged, while a tree inside the bounds is walked in full.
func Test_ContractDeployProcessor_Process_oversizedAuthTreeNeverFails(t *testing.T) {
	create, contractID := declaredCreate(t)
	wide := func(siblings int) xdr.SorobanAuthorizedInvocation {
		subs := make([]xdr.SorobanAuthorizedInvocation, 0, siblings+1)
		for i := 0; i < siblings; i++ {
			subs = append(subs, contractFnNode())
		}
		return contractFnNode(append(subs, create)...)
	}

	testCases := []struct {
		name        string
		roots       []xdr.SorobanAuthorizedInvocation
		wantDeploy  bool
		wantTruncBy string
	}{
		{
			name:       "65_deep_chain_is_walked_in_full",
			roots:      []xdr.SorobanAuthorizedInvocation{chainAbove(create, 65)},
			wantDeploy: true,
		},
		{
			// The deepest tree stellar-core v30.0.0 applies is 245 invocations in one chain.
			name:       "245_deep_chain_the_deepest_core_applies_is_walked_in_full",
			roots:      []xdr.SorobanAuthorizedInvocation{chainAbove(create, 244)},
			wantDeploy: true,
		},
		{
			// A 246-deep chain is included in a ledger too (today its transaction fails).
			name:       "246_deep_chain_is_walked_in_full",
			roots:      []xdr.SorobanAuthorizedInvocation{chainAbove(create, 245)},
			wantDeploy: true,
		},
		{
			name:       "deploy_on_the_deepest_level_walked",
			roots:      []xdr.SorobanAuthorizedInvocation{chainAbove(create, maxAuthWalkDepth-1)},
			wantDeploy: true,
		},
		{
			name:        "deploy_one_level_past_the_depth_bound_is_skipped",
			roots:       []xdr.SorobanAuthorizedInvocation{chainAbove(create, maxAuthWalkDepth)},
			wantTruncBy: "depth",
		},
		{
			name:        "chain_far_past_the_depth_bound_is_skipped",
			roots:       []xdr.SorobanAuthorizedInvocation{chainAbove(create, maxAuthWalkDepth+300)},
			wantTruncBy: "depth",
		},
		{
			name:       "wide_tree_inside_the_node_bound_is_walked_in_full",
			roots:      []xdr.SorobanAuthorizedInvocation{wide(maxAuthWalkNodes - 2)},
			wantDeploy: true,
		},
		{
			name:        "wide_tree_past_the_node_bound_is_skipped",
			roots:       []xdr.SorobanAuthorizedInvocation{wide(maxAuthWalkNodes + 1000)},
			wantTruncBy: "nodes",
		},
		{
			name: "node_bound_spans_the_auth_entries_of_one_operation",
			roots: []xdr.SorobanAuthorizedInvocation{
				wide(maxAuthWalkNodes/2 - 2).SubInvocations[0], // one node
				chainAbove(contractFnNode(), 10),
				contractFnNode(wide(maxAuthWalkNodes).SubInvocations[:maxAuthWalkNodes]...),
				create,
			},
			wantTruncBy: "nodes",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ingestionMetrics := metrics.NewMetrics(prometheus.NewRegistry()).Ingestion
			proc := NewContractDeployProcessor(network.TestNetworkPassphrase, ingestionMetrics)
			op := opWithAuthRoots(t, contractID, tc.roots...)

			stateChanges, err := proc.ProcessOperation(context.Background(), op)
			require.NoError(t, err, "an oversized declared auth tree must never fail the operation")

			if tc.wantDeploy {
				assertStateChangesElementsMatch(t, []types.StateChange{
					NewStateChangeBuilder(12345, closeTime.Unix(), op.TransactionID()).
						WithOperationID(op.ID()).
						WithReason(types.StateChangeReasonCreate).
						WithCategory(types.StateChangeCategoryAccount).
						WithCreator(deployerAccountID).
						WithAccount(contractID).
						Build(),
				}, stateChanges)
			} else {
				assert.Empty(t, stateChanges)
			}

			for _, bound := range []string{"depth", "nodes"} {
				want := 0.0
				if bound == tc.wantTruncBy {
					want = 1.0
				}
				assert.Equal(t, want, testutil.ToFloat64(ingestionMetrics.AuthTreeWalkTruncationsTotal.WithLabelValues(bound)), "truncations counted for bound %q", bound)
			}
		})
	}
}

// Test_ContractDeployProcessor_Process_noCreatedInstanceSkipsAuthWalk: with no created
// instance in the meta nothing can be confirmed, so the declared trees are not walked at
// all, however large, and nothing is counted as truncated.
func Test_ContractDeployProcessor_Process_noCreatedInstanceSkipsAuthWalk(t *testing.T) {
	create, contractID := declaredCreate(t)
	ingestionMetrics := metrics.NewMetrics(prometheus.NewRegistry()).Ingestion
	proc := NewContractDeployProcessor(network.TestNetworkPassphrase, ingestionMetrics)
	op := opWithAuthRoots(t, contractID, chainAbove(create, maxAuthWalkDepth+300))
	setOperationMeta(op, nil, nil)

	stateChanges, err := proc.ProcessOperation(context.Background(), op)
	require.NoError(t, err)
	assert.Empty(t, stateChanges)
	assert.Equal(t, 0.0, testutil.ToFloat64(ingestionMetrics.AuthTreeWalkTruncationsTotal.WithLabelValues("depth")))
}
