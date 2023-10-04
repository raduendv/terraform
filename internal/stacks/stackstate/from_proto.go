// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package stackstate

import (
	"fmt"

	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/stacks/stackaddrs"
	"github.com/hashicorp/terraform/internal/stacks/stackstate/statekeys"
	"github.com/hashicorp/terraform/internal/stacks/tfstackdata1"
	"github.com/hashicorp/terraform/internal/states"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/anypb"
)

func LoadFromProto(msgs map[string]*anypb.Any) (*State, error) {
	ret := NewState()
	for rawKey, rawMsg := range msgs {
		key, err := statekeys.Parse(rawKey)
		if err != nil {
			// "invalid" here means that it was either not syntactically
			// valid at all or was a recognized type but with the wrong
			// syntax for that type.
			// An unrecognized key type is NOT invalid; we handle that below.
			return nil, fmt.Errorf("invalid tracking key %q in state: %w", rawKey, err)
		}

		if !statekeys.RecognizedType(key) {
			// There are three different strategies for dealing with
			// unrecognized keys, which we recognize based on naming
			// conventions of the key types.
			switch handling := key.KeyType().UnrecognizedKeyHandling(); handling {

			case statekeys.FailIfUnrecognized:
				// This is for keys whose messages materially change the
				// meaning of the state and so cannot be ignored. Keys
				// with this treatment are forwards-incompatible (old versions
				// of Terraform will fail to load a state containing them) so
				// should be added only as a last resort.
				return nil, fmt.Errorf("state was created by a newer version of Terraform Core (unrecognized tracking key %q)", rawKey)

			case statekeys.PreserveIfUnrecognized:
				// This is for keys whose messages can safely be left entirely
				// unchanged if applying a plan with a version of Terraform
				// that doesn't understand them. Keys in this category should
				// typically be standalone and not refer to or depend on any
				// other objects in the state, to ensure that removing or
				// updating other objects will not cause the preserved message
				// to become misleading or invalid.
				// We don't need to do anything special with these ones because
				// the caller should preserve any object we don't explicitly
				// update or delete during the apply phase.

			case statekeys.DiscardIfUnrecognized:
				// This is for keys which can be discarded when planning or
				// applying with an older version of Terraform that doesn't
				// understand them. This category is for optional ancillary
				// information -- not actually required for correct subsequent
				// planning -- especially if it could be recomputed again and
				// repopulated if later planning and applying with a newer
				// version of Terraform Core.
				// For these ones we need to remember their keys so that we
				// can emit "delete" messages early in the apply phase to
				// actually discard them from the caller's records.
				ret.discardUnsupportedKeys.Add(key)

			default:
				// Should not get here. The above should be exhaustive.
				panic(fmt.Sprintf("unsupported UnrecognizedKeyHandling value %s", handling))
			}
			continue
		}

		msg, err := anypb.UnmarshalNew(rawMsg, proto.UnmarshalOptions{})
		if err != nil {
			return nil, fmt.Errorf("invalid raw value for raw state key %q: %w", rawKey, err)
		}

		switch key := key.(type) {

		case statekeys.ComponentInstance:
			err := handleComponentInstanceMsg(key, msg, ret)
			if err != nil {
				return nil, err
			}

		case statekeys.ResourceInstanceObject:
			err := handleResourceInstanceObjectMsg(key, msg, ret)
			if err != nil {
				return nil, err
			}

		default:
			// Should not get here: the above should be exhaustive for all
			// possible key types.
			panic(fmt.Sprintf("unsupported state key type %T", key))
		}
	}
	return ret, nil
}

func handleComponentInstanceMsg(key statekeys.ComponentInstance, msg protoreflect.ProtoMessage, state *State) error {
	// For this particular object type all of the information is in the key,
	// for now at least.
	_, ok := msg.(*tfstackdata1.StateComponentInstanceV1)
	if !ok {
		return fmt.Errorf("unsupported message type %T for %s state", msg, key.ComponentInstanceAddr)
	}

	state.ensureComponentInstanceState(key.ComponentInstanceAddr)
	return nil
}

func handleResourceInstanceObjectMsg(key statekeys.ResourceInstanceObject, msg protoreflect.ProtoMessage, state *State) error {
	fullAddr := stackaddrs.AbsResourceInstanceObject{
		Component: key.ResourceInstance.Component,
		Item: addrs.AbsResourceInstanceObject{
			ResourceInstance: key.ResourceInstance.Item,
			DeposedKey:       key.DeposedKey,
		},
	}

	riMsg, ok := msg.(*tfstackdata1.StateResourceInstanceObjectV1)
	if !ok {
		return fmt.Errorf("unsupported message type %T for state of %s", msg, fullAddr.String())
	}

	objSrc := &states.ResourceInstanceObjectSrc{
		SchemaVersion:       riMsg.SchemaVersion,
		AttrsJSON:           riMsg.ValueJson,
		CreateBeforeDestroy: riMsg.CreateBeforeDestroy,
		Private:             riMsg.ProviderSpecificData,
	}

	switch riMsg.Status {
	case tfstackdata1.StateResourceInstanceObjectV1_READY:
		objSrc.Status = states.ObjectReady
	case tfstackdata1.StateResourceInstanceObjectV1_DAMAGED:
		objSrc.Status = states.ObjectTainted
	default:
		return fmt.Errorf("unsupported status %s for %s", riMsg.Status.String(), fullAddr.String())
	}

	providerConfigAddr, diags := addrs.ParseAbsProviderConfigStr(riMsg.ProviderConfigAddr)
	if diags.HasErrors() {
		return fmt.Errorf("provider configuration reference %q for %s", riMsg.ProviderConfigAddr, fullAddr)
	}

	if len(riMsg.SensitivePaths) != 0 {
		// TODO: Deal with sensitive paths
	}

	if len(riMsg.Dependencies) != 0 {
		objSrc.Dependencies = make([]addrs.ConfigResource, len(riMsg.Dependencies))
		for i, raw := range riMsg.Dependencies {
			instAddr, diags := addrs.ParseAbsResourceInstanceStr(raw)
			if diags.HasErrors() {
				return fmt.Errorf("invalid dependency %q for %s", raw, fullAddr)
			}
			// We used the resource instance address parser here but we
			// actually want the "config resource" subset of that syntax only.
			configAddr := instAddr.ConfigResource()
			if configAddr.String() != instAddr.String() {
				return fmt.Errorf("invalid dependency %q for %s", raw, fullAddr)
			}
			objSrc.Dependencies[i] = configAddr
		}
	}

	state.addResourceInstanceObject(fullAddr, objSrc, providerConfigAddr)
	return nil
}
