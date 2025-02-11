package full

import (
	"bytes"

	"github.com/multiformats/go-multicodec"
	cbg "github.com/whyrusleeping/cbor-gen"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-state-types/builtin"
	"github.com/filecoin-project/go-state-types/builtin/v10/evm"

	builtinactors "github.com/filecoin-project/lotus/chain/actors/builtin"
	"github.com/filecoin-project/lotus/chain/state"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/filecoin-project/lotus/chain/types/ethtypes"
)

// decodePayload is a utility function which decodes the payload using the given codec
func decodePayload(payload []byte, codec uint64) (ethtypes.EthBytes, error) {
	if len(payload) == 0 {
		return nil, nil
	}

	switch multicodec.Code(codec) {
	case multicodec.Identity:
		return nil, nil
	case multicodec.DagCbor, multicodec.Cbor:
		buf, err := cbg.ReadByteArray(bytes.NewReader(payload), uint64(len(payload)))
		if err != nil {
			return nil, xerrors.Errorf("decodePayload: failed to decode cbor payload: %w", err)
		}
		return buf, nil
	case multicodec.Raw:
		return ethtypes.EthBytes(payload), nil
	}

	return nil, xerrors.Errorf("decodePayload: unsupported codec: %d", codec)
}

// buildTraces recursively builds the traces for a given ExecutionTrace by walking the subcalls
func buildTraces(traces *[]*ethtypes.EthTrace, parent *ethtypes.EthTrace, addr []int, et types.ExecutionTrace, height int64, st *state.StateTree) error {
	// lookup the eth address from the from/to addresses. Note that this may fail but to support
	// this we need to include the ActorID in the trace. For now, just log a warning and skip
	// this trace.
	//
	// TODO: Add ActorID in trace, see https://github.com/filecoin-project/lotus/pull/11100#discussion_r1302442288
	from, err := lookupEthAddress(et.Msg.From, st)
	if err != nil {
		log.Warnf("buildTraces: failed to lookup from address %s: %v", et.Msg.From, err)
		return nil
	}
	to, err := lookupEthAddress(et.Msg.To, st)
	if err != nil {
		log.Warnf("buildTraces: failed to lookup to address %s: %w", et.Msg.To, err)
		return nil
	}

	// Skip the trace if we never reached the point where we invoked this actor.
	if et.InvokedActor == nil {
		return nil
	}

	trace := &ethtypes.EthTrace{
		Action: ethtypes.EthTraceAction{
			From:  from,
			To:    to,
			Gas:   ethtypes.EthUint64(et.Msg.GasLimit),
			Input: nil,
			Value: ethtypes.EthBigInt(et.Msg.Value),

			FilecoinFrom:    et.Msg.From,
			FilecoinTo:      et.Msg.To,
			FilecoinMethod:  et.Msg.Method,
			FilecoinCodeCid: et.InvokedActor.State.Code,
		},
		Result: ethtypes.EthTraceResult{
			GasUsed: ethtypes.EthUint64(et.SumGas().TotalGas),
			Output:  nil,
		},
		Subtraces:    0, // will be updated by the children once they are added to the trace
		TraceAddress: addr,

		Parent:       parent,
		LastByteCode: nil,
	}

	trace.SetCallType("call")

	if et.Msg.Method == builtin.MethodsEVM.InvokeContract {
		log.Debugf("COND1 found InvokeContract call at height: %d", height)

		// TODO: ignore return errors since actors can send gibberish and we don't want
		// to fail the whole trace in that case
		trace.Action.Input, err = decodePayload(et.Msg.Params, et.Msg.ParamsCodec)
		if err != nil {
			return xerrors.Errorf("buildTraces: %w", err)
		}
		trace.Result.Output, err = decodePayload(et.MsgRct.Return, et.MsgRct.ReturnCodec)
		if err != nil {
			return xerrors.Errorf("buildTraces: %w", err)
		}
	} else if et.Msg.To == builtin.EthereumAddressManagerActorAddr &&
		et.Msg.Method == builtin.MethodsEAM.CreateExternal {
		log.Debugf("COND2 found CreateExternal call at height: %d", height)
		trace.Action.Input, err = decodePayload(et.Msg.Params, et.Msg.ParamsCodec)
		if err != nil {
			return xerrors.Errorf("buildTraces: %w", err)
		}

		if et.MsgRct.ExitCode.IsSuccess() {
			// ignore return value
			trace.Result.Output = nil
		} else {
			// return value is the error message
			trace.Result.Output, err = decodePayload(et.MsgRct.Return, et.MsgRct.ReturnCodec)
			if err != nil {
				return xerrors.Errorf("buildTraces: %w", err)
			}
		}

		// treat this as a contract creation
		trace.SetCallType("create")
	} else {
		// we are going to assume a native method, but we may change it in one of the edge cases below
		// TODO: only do this if we know it's a native method (optimization)
		trace.Action.Input = encodeFilecoinParamsAsABI(et.Msg.Method, et.Msg.ParamsCodec, et.Msg.Params)
		trace.Result.Output = encodeFilecoinReturnAsABI(et.MsgRct.ExitCode, et.MsgRct.ReturnCodec, et.MsgRct.Return)
	}

	// TODO: is it OK to check this here or is this only specific to certain edge case (evm to evm)?
	if et.Msg.ReadOnly {
		trace.SetCallType("staticcall")
	}

	// there are several edge cases that require special handling when displaying the traces. Note that while iterating over
	// the traces we update the trace backwards (through the parent pointer)
	if parent != nil {
		// Handle Native actor creation
		//
		// Actor A calls to the init actor on method 2 and The init actor creates the target actor B then calls it on method 1
		if parent.Action.FilecoinTo == builtin.InitActorAddr &&
			parent.Action.FilecoinMethod == builtin.MethodsInit.Exec &&
			et.Msg.Method == builtin.MethodConstructor {
			log.Debugf("COND3 Native actor creation! method:%d, code:%s, height:%d", et.Msg.Method, et.InvokedActor.State.Code.String(), height)
			parent.SetCallType("create")
			parent.Action.To = to
			parent.Action.Input = []byte{0xFE}
			parent.Result.Output = nil

			// there should never be any subcalls when creating a native actor
			//
			// TODO: add support for native actors calling another when created
			return nil
		}

		// Handle EVM contract creation
		//
		// To detect EVM contract creation we need to check for the following sequence of events:
		//
		// 1) EVM contract A calls the EAM (Ethereum Address Manager) on method 2 (create) or 3 (create2).
		// 2) The EAM calls the init actor on method 3 (Exec4).
		// 3) The init actor creates the target actor B then calls it on method 1.
		if parent.Parent != nil {
			calledCreateOnEAM := parent.Parent.Action.FilecoinTo == builtin.EthereumAddressManagerActorAddr &&
				(parent.Parent.Action.FilecoinMethod == builtin.MethodsEAM.Create || parent.Parent.Action.FilecoinMethod == builtin.MethodsEAM.Create2)
			eamCalledInitOnExec4 := parent.Action.FilecoinTo == builtin.InitActorAddr &&
				parent.Action.FilecoinMethod == builtin.MethodsInit.Exec4
			initCreatedActor := trace.Action.FilecoinMethod == builtin.MethodConstructor

			// TODO: We need to handle failures in contract creations and support resurrections on an existing but dead EVM actor)
			if calledCreateOnEAM && eamCalledInitOnExec4 && initCreatedActor {
				log.Debugf("COND4 EVM contract creation method:%d, code:%s, height:%d", et.Msg.Method, et.InvokedActor.State.Code.String(), height)

				if parent.Parent.Action.FilecoinMethod == builtin.MethodsEAM.Create {
					parent.Parent.SetCallType("create")
				} else {
					parent.Parent.SetCallType("create2")
				}

				// update the parent.parent to make this
				parent.Parent.Action.To = trace.Action.To
				parent.Parent.Subtraces = 0

				// delete the parent (the EAM) and skip the current trace (init)
				*traces = (*traces)[:len(*traces)-1]

				return nil
			}
		}

		if builtinactors.IsEvmActor(parent.Action.FilecoinCodeCid) {
			// Handle delegate calls
			//
			// 1) Look for trace from an EVM actor to itself on InvokeContractDelegate, method 6.
			// 2) Check that the previous trace calls another actor on method 3 (GetByteCode) and they are at the same level (same parent)
			// 3) Treat this as a delegate call to actor A.
			if parent.LastByteCode != nil && trace.Action.From == trace.Action.To &&
				trace.Action.FilecoinMethod == builtin.MethodsEVM.InvokeContractDelegate {
				log.Debugf("COND7 found delegate call, height: %d", height)
				prev := parent.LastByteCode
				if prev.Action.From == trace.Action.From && prev.Action.FilecoinMethod == builtin.MethodsEVM.GetBytecode && prev.Parent == trace.Parent {
					trace.SetCallType("delegatecall")
					trace.Action.To = prev.Action.To

					var dp evm.DelegateCallParams
					err := dp.UnmarshalCBOR(bytes.NewReader(et.Msg.Params))
					if err != nil {
						return xerrors.Errorf("failed UnmarshalCBOR: %w", err)
					}
					trace.Action.Input = dp.Input

					trace.Result.Output, err = decodePayload(et.MsgRct.Return, et.MsgRct.ReturnCodec)
					if err != nil {
						return xerrors.Errorf("failed decodePayload: %w", err)
					}
				}
			} else {
				// Handle EVM call special casing
				//
				// Any outbound call from an EVM actor on methods 1-1023 are side-effects from EVM instructions
				// and should be dropped from the trace.
				if et.Msg.Method > 0 &&
					et.Msg.Method <= 1023 {
					log.Debugf("Infof found outbound call from an EVM actor on method 1-1023 method:%d, code:%s, height:%d", et.Msg.Method, parent.Action.FilecoinCodeCid.String(), height)

					if et.Msg.Method == builtin.MethodsEVM.GetBytecode {
						// save the last bytecode trace to handle delegate calls
						parent.LastByteCode = trace
					}

					return nil
				}
			}
		}

	}

	// we are adding trace to the traces so update the parent subtraces count as it was originally set to zero
	if parent != nil {
		parent.Subtraces++
	}

	*traces = append(*traces, trace)

	for i, call := range et.Subcalls {
		err := buildTraces(traces, trace, append(addr, i), call, height, st)
		if err != nil {
			return err
		}
	}

	return nil
}
