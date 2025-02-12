// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package stackmigrate

import (
	stdcmp "cmp"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/hashicorp/go-slug/sourceaddrs"
	"github.com/hashicorp/go-slug/sourcebundle"
	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/collections"
	"github.com/hashicorp/terraform/internal/depsfile"
	"github.com/hashicorp/terraform/internal/getproviders/providerreqs"
	"github.com/hashicorp/terraform/internal/plans"
	"github.com/hashicorp/terraform/internal/providers"
	"github.com/hashicorp/terraform/internal/stacks/stackaddrs"
	"github.com/hashicorp/terraform/internal/stacks/stackconfig"
	"github.com/hashicorp/terraform/internal/stacks/stackplan"
	"github.com/hashicorp/terraform/internal/stacks/stackruntime"
	stacks_testing_provider "github.com/hashicorp/terraform/internal/stacks/stackruntime/testing"
	"github.com/hashicorp/terraform/internal/stacks/stackstate"
	"github.com/hashicorp/terraform/internal/states"
	"github.com/hashicorp/terraform/internal/tfdiags"
	"github.com/zclconf/go-cty-debug/ctydebug"
	"github.com/zclconf/go-cty/cty"
)

func TestMigrate_Module(t *testing.T) {
	cfg := loadMainBundleConfigForTest(t, filepath.Join("with-single-input", "valid"))

	lock := depsfile.NewLocks()
	lock.SetProvider(
		addrs.NewDefaultProvider("testing"),
		providerreqs.MustParseVersion("0.0.0"),
		providerreqs.MustParseVersionConstraints("=0.0.0"),
		providerreqs.PreferredHashes([]providerreqs.Hash{}),
	)

	state := states.BuildState(func(ss *states.SyncState) {
		ss.SetOutputValue(addrs.AbsOutputValue{
			Module:      addrs.RootModuleInstance,
			OutputValue: addrs.OutputValue{Name: "output"},
		}, cty.StringVal("before"), false)
	})
	rootModule := state.RootModule()
	rootModule.SetResourceInstanceCurrent(
		addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "testing_resource",
			Name: "data",
		}.Instance(addrs.NoKey),
		&states.ResourceInstanceObjectSrc{
			Status: states.ObjectReady,
			AttrsJSON: []byte(`{
				"id": "foo",
				"value": "hello"
			}`),
		},
		mustDefaultRootProvider("testing"),
	)
	rootModule.SetResourceInstanceDeposed(
		addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "testing_resource",
			Name: "data",
		}.Instance(addrs.NoKey),
		states.NewDeposedKey(),
		&states.ResourceInstanceObjectSrc{
			Status: states.ObjectReady,
			AttrsJSON: []byte(`{
				"id": "foo",
				"value": "hello"
			}`),
		},
		mustDefaultRootProvider("testing"),
	)
	mig := Migration{
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("testing"): func() (providers.Interface, error) {
				return stacks_testing_provider.NewProvider(t), nil
			},
		},
		PreviousState: state,
		Config:        cfg,
	}
	resources := map[string]string{}
	modules := map[string]string{"": "self"}

	applied := []stackstate.AppliedChange{}
	expected := []stackstate.AppliedChange{
		&stackstate.AppliedChangeResourceInstanceObject{
			ResourceInstanceObjectAddr: mustAbsResourceInstanceObject("component.self.testing_resource.data"),
			NewStateSrc: &states.ResourceInstanceObjectSrc{
				AttrsJSON: mustMarshalJSONAttrs(map[string]interface{}{
					"id":    "foo",
					"value": "hello",
				}),
				Status:  states.ObjectReady,
				Private: nil,
			},
			ProviderConfigAddr: mustDefaultRootProvider("testing"),
			Schema:             stacks_testing_provider.TestingResourceSchema,
		},
		&stackstate.AppliedChangeResourceInstanceObject{
			ResourceInstanceObjectAddr: stackaddrs.AbsResourceInstanceObject{
				Component: mustAbsResourceInstanceObject("component.self.testing_resource.data").Component,
				Item: addrs.AbsResourceInstanceObject{
					ResourceInstance: mustAbsResourceInstanceObject("component.self.testing_resource.data").Item.ResourceInstance,
					DeposedKey:       states.NewDeposedKey(),
				},
			},
			NewStateSrc: &states.ResourceInstanceObjectSrc{
				AttrsJSON: mustMarshalJSONAttrs(map[string]interface{}{
					"id":    "foo",
					"value": "hello",
				}),
				Status:  states.ObjectReady,
				Private: nil,
			},
			ProviderConfigAddr: mustDefaultRootProvider("testing"),
			Schema:             stacks_testing_provider.TestingResourceSchema,
		},
		&stackstate.AppliedChangeComponentInstance{
			ComponentAddr:         mustAbsComponent("component.self"),
			ComponentInstanceAddr: mustAbsComponentInstance("component.self"),
			OutputValues:          map[addrs.OutputValue]cty.Value{},
			InputVariables: map[addrs.InputVariable]cty.Value{
				{Name: "id"}:    cty.DynamicVal,
				{Name: "input"}: cty.DynamicVal,
			},
		},
	}

	var expDiags, gotDiags tfdiags.Diagnostics
	mig.Migrate(resources, modules, func(change stackstate.AppliedChange) {
		applied = append(applied, change)
	}, func(diagnostic tfdiags.Diagnostic) {
		gotDiags = append(gotDiags, diagnostic)
	})

	if diff := cmp.Diff(expected, applied, changesCmpOpts, cmpopts.IgnoreFields(
		addrs.AbsResourceInstanceObject{}, "DeposedKey",
	)); diff != "" {
		t.Fatalf("unexpected applied changes:\n%s", diff)
	}

	if diff := cmp.Diff(expDiags, gotDiags); diff != "" {
		t.Fatalf("unexpected diagnostics:\n%s", diff)
	}
}

func TestMigrate_RootResources(t *testing.T) {
	cfg := loadMainBundleConfigForTest(t, filepath.Join("with-single-input", "valid"))

	lock := depsfile.NewLocks()
	lock.SetProvider(
		addrs.NewDefaultProvider("testing"),
		providerreqs.MustParseVersion("0.0.0"),
		providerreqs.MustParseVersionConstraints("=0.0.0"),
		providerreqs.PreferredHashes([]providerreqs.Hash{}),
	)

	state := states.BuildState(func(ss *states.SyncState) {
		ss.SetOutputValue(addrs.AbsOutputValue{
			Module:      addrs.RootModuleInstance,
			OutputValue: addrs.OutputValue{Name: "output"},
		}, cty.StringVal("before"), false)
	})
	rootModule := state.RootModule()
	rootModule.SetResourceInstanceCurrent(
		addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "testing_resource",
			Name: "data",
		}.Instance(addrs.NoKey),
		&states.ResourceInstanceObjectSrc{
			Status: states.ObjectReady,
			AttrsJSON: []byte(`{
				"id": "foo",
				"value": "hello"
			}`),
		},
		mustDefaultRootProvider("testing"),
	)
	rootModule.SetResourceInstanceDeposed(
		addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "testing_resource",
			Name: "data",
		}.Instance(addrs.NoKey),
		states.NewDeposedKey(),
		&states.ResourceInstanceObjectSrc{
			Status: states.ObjectReady,
			AttrsJSON: []byte(`{
				"id": "foo",
				"value": "hello"
			}`),
		},
		mustDefaultRootProvider("testing"),
	)
	mig := Migration{
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("testing"): func() (providers.Interface, error) {
				return stacks_testing_provider.NewProvider(t), nil
			},
		},
		PreviousState: state,
		Config:        cfg,
	}
	resources := map[string]string{
		"testing_resource.data": "self",
	}
	modules := map[string]string{}

	applied := []stackstate.AppliedChange{}
	expected := []stackstate.AppliedChange{
		&stackstate.AppliedChangeResourceInstanceObject{
			ResourceInstanceObjectAddr: mustAbsResourceInstanceObject("component.self.testing_resource.data"),
			NewStateSrc: &states.ResourceInstanceObjectSrc{
				AttrsJSON: mustMarshalJSONAttrs(map[string]interface{}{
					"id":    "foo",
					"value": "hello",
				}),
				Status:  states.ObjectReady,
				Private: nil,
			},
			ProviderConfigAddr: mustDefaultRootProvider("testing"),
			Schema:             stacks_testing_provider.TestingResourceSchema,
		},
		&stackstate.AppliedChangeResourceInstanceObject{
			ResourceInstanceObjectAddr: stackaddrs.AbsResourceInstanceObject{
				Component: mustAbsResourceInstanceObject("component.self.testing_resource.data").Component,
				Item: addrs.AbsResourceInstanceObject{
					ResourceInstance: mustAbsResourceInstanceObject("component.self.testing_resource.data").Item.ResourceInstance,
					DeposedKey:       states.NewDeposedKey(),
				},
			},
			NewStateSrc: &states.ResourceInstanceObjectSrc{
				AttrsJSON: mustMarshalJSONAttrs(map[string]interface{}{
					"id":    "foo",
					"value": "hello",
				}),
				Status:  states.ObjectReady,
				Private: nil,
			},
			ProviderConfigAddr: mustDefaultRootProvider("testing"),
			Schema:             stacks_testing_provider.TestingResourceSchema,
		},
		&stackstate.AppliedChangeComponentInstance{
			ComponentAddr:         mustAbsComponent("component.self"),
			ComponentInstanceAddr: mustAbsComponentInstance("component.self"),
			OutputValues:          map[addrs.OutputValue]cty.Value{},
			InputVariables: map[addrs.InputVariable]cty.Value{
				{Name: "id"}:    cty.DynamicVal,
				{Name: "input"}: cty.DynamicVal,
			},
		},
	}

	var expDiags, gotDiags tfdiags.Diagnostics
	mig.Migrate(resources, modules, func(change stackstate.AppliedChange) {
		applied = append(applied, change)
	}, func(diagnostic tfdiags.Diagnostic) {
		gotDiags = append(gotDiags, diagnostic)
	})

	if diff := cmp.Diff(expected, applied, changesCmpOpts, cmpopts.IgnoreFields(
		addrs.AbsResourceInstanceObject{}, "DeposedKey",
	)); diff != "" {
		t.Fatalf("unexpected applied changes:\n%s", diff)
	}

	if diff := cmp.Diff(expDiags, gotDiags); diff != "" {
		t.Fatalf("unexpected diagnostics:\n%s", diff)
	}
}

func TestMigrate_ComponentDependency(t *testing.T) {
	cfg := loadMainBundleConfigForTest(t, filepath.Join("for-stacks-migrate", "with-dependency", "input-dependency"))

	lock := depsfile.NewLocks()
	lock.SetProvider(
		addrs.NewDefaultProvider("testing"),
		providerreqs.MustParseVersion("0.0.0"),
		providerreqs.MustParseVersionConstraints("=0.0.0"),
		providerreqs.PreferredHashes([]providerreqs.Hash{}),
	)

	state := states.BuildState(func(ss *states.SyncState) {
		ss.SetOutputValue(addrs.AbsOutputValue{
			Module:      addrs.RootModuleInstance,
			OutputValue: addrs.OutputValue{Name: "output"},
		}, cty.StringVal("before"), false)
	})
	rootModule := state.RootModule()
	rootModule.SetResourceInstanceCurrent(
		addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "testing_resource",
			Name: "data",
		}.Instance(addrs.NoKey),
		&states.ResourceInstanceObjectSrc{
			Status: states.ObjectReady,
			AttrsJSON: []byte(`{
				"id": "foo",
				"value": "hello"
			}`),
		},
		mustDefaultRootProvider("testing"),
	)
	rootModule.SetResourceInstanceCurrent(
		addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "testing_resource",
			Name: "another",
		}.Instance(addrs.IntKey(0)),
		&states.ResourceInstanceObjectSrc{
			Status: states.ObjectReady,
			AttrsJSON: []byte(`{
				"id": "foo",
				"value": "hello"
			}`),
		},
		mustDefaultRootProvider("testing"),
	)
	rootModule.SetResourceInstanceCurrent(
		addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "testing_resource",
			Name: "another",
		}.Instance(addrs.IntKey(1)),
		&states.ResourceInstanceObjectSrc{
			Status: states.ObjectReady,
			AttrsJSON: []byte(`{
				"id": "foo",
				"value": "hello"
			}`),
		},
		mustDefaultRootProvider("testing"),
	)

	mig := Migration{
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("testing"): func() (providers.Interface, error) {
				return stacks_testing_provider.NewProvider(t), nil
			},
		},
		PreviousState: state,
		Config:        cfg,
	}
	resources := map[string]string{
		"testing_resource.data":    "parent",
		"testing_resource.another": "child",
	}
	modules := map[string]string{}

	appliedResources := []*stackstate.AppliedChangeResourceInstanceObject{}
	appliedComponents := []*stackstate.AppliedChangeComponentInstance{}
	expectedResources := []*stackstate.AppliedChangeResourceInstanceObject{
		{
			ResourceInstanceObjectAddr: mustAbsResourceInstanceObject("component.parent.testing_resource.data"),
			NewStateSrc: &states.ResourceInstanceObjectSrc{
				AttrsJSON: mustMarshalJSONAttrs(map[string]interface{}{
					"id":    "foo",
					"value": "hello",
				}),
				Status:  states.ObjectReady,
				Private: nil,
			},
			ProviderConfigAddr: mustDefaultRootProvider("testing"),
			Schema:             stacks_testing_provider.TestingResourceSchema,
		},
		{
			ResourceInstanceObjectAddr: mustAbsResourceInstanceObject("component.child.testing_resource.another[0]"),
			NewStateSrc: &states.ResourceInstanceObjectSrc{
				AttrsJSON: mustMarshalJSONAttrs(map[string]interface{}{
					"id":    "foo",
					"value": "hello",
				}),
				Status:  states.ObjectReady,
				Private: nil,
			},
			ProviderConfigAddr: mustDefaultRootProvider("testing"),
			Schema:             stacks_testing_provider.TestingResourceSchema,
		},
		{
			ResourceInstanceObjectAddr: mustAbsResourceInstanceObject("component.child.testing_resource.another[1]"),
			NewStateSrc: &states.ResourceInstanceObjectSrc{
				AttrsJSON: mustMarshalJSONAttrs(map[string]interface{}{
					"id":    "foo",
					"value": "hello",
				}),
				Status:  states.ObjectReady,
				Private: nil,
			},
			ProviderConfigAddr: mustDefaultRootProvider("testing"),
			Schema:             stacks_testing_provider.TestingResourceSchema,
		},
	}
	expectedComponents := []*stackstate.AppliedChangeComponentInstance{
		{
			ComponentAddr:         mustAbsComponent("component.parent"),
			ComponentInstanceAddr: mustAbsComponentInstance("component.parent"),
			OutputValues: map[addrs.OutputValue]cty.Value{
				{Name: "id"}: cty.DynamicVal,
			},
			InputVariables: map[addrs.InputVariable]cty.Value{
				{Name: "id"}:    cty.DynamicVal,
				{Name: "input"}: cty.DynamicVal,
			},
			Dependents: collections.NewSet(mustAbsComponent("component.child")),
		},
		{
			ComponentAddr:         mustAbsComponent("component.child"),
			ComponentInstanceAddr: mustAbsComponentInstance("component.child"),
			OutputValues: map[addrs.OutputValue]cty.Value{
				{Name: "id"}: cty.DynamicVal,
			},
			InputVariables: map[addrs.InputVariable]cty.Value{
				{Name: "id"}:    cty.DynamicVal,
				{Name: "input"}: cty.DynamicVal,
			},
			Dependencies: collections.NewSet(mustAbsComponent("component.parent")),
		},
	}

	var expDiags, gotDiags tfdiags.Diagnostics
	mig.Migrate(resources, modules, func(change stackstate.AppliedChange) {
		switch c := change.(type) {
		case *stackstate.AppliedChangeResourceInstanceObject:
			appliedResources = append(appliedResources, c)
		case *stackstate.AppliedChangeComponentInstance:
			appliedComponents = append(appliedComponents, c)
		}
	}, func(diagnostic tfdiags.Diagnostic) {
		gotDiags = append(gotDiags, diagnostic)
	})

	if diff := compareAppliedChanges(t, expectedResources, appliedResources, func(c *stackstate.AppliedChangeResourceInstanceObject) string {
		return c.ResourceInstanceObjectAddr.String()
	}); diff != "" {
		t.Fatalf("unexpected applied resource changes:\n%s", diff)
	}

	if diff := compareAppliedChanges(t, expectedComponents, appliedComponents, func(c *stackstate.AppliedChangeComponentInstance) string {
		return c.ComponentAddr.String()
	}); diff != "" {
		t.Fatalf("unexpected applied component changes:\n%s", diff)
	}

	if diff := cmp.Diff(expDiags, gotDiags); diff != "" {
		t.Fatalf("unexpected diagnostics:\n%s", diff)
	}
}

func TestMigrateConfig_NestedResource(t *testing.T) {
	cfg := loadMainBundleConfigForTest(t, filepath.Join("for-stacks-migrate", "with-nested-module"))

	lock := depsfile.NewLocks()
	lock.SetProvider(
		addrs.NewDefaultProvider("testing"),
		providerreqs.MustParseVersion("0.0.0"),
		providerreqs.MustParseVersionConstraints("=0.0.0"),
		providerreqs.PreferredHashes([]providerreqs.Hash{}),
	)

	state := states.BuildState(func(ss *states.SyncState) {
		ss.SetOutputValue(addrs.AbsOutputValue{
			Module:      addrs.RootModuleInstance,
			OutputValue: addrs.OutputValue{Name: "output"},
		}, cty.StringVal("before"), false)
	})
	rootModule := state.RootModule()
	rootModule.SetResourceInstanceCurrent(
		addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "testing_resource",
			Name: "data",
		}.Instance(addrs.NoKey),
		&states.ResourceInstanceObjectSrc{
			Status: states.ObjectReady,
			AttrsJSON: []byte(`{
				"id": "foo",
				"value": "hello"
			}`),
		},
		mustDefaultRootProvider("testing"),
	)
	rootModule.SetResourceInstanceCurrent(
		addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "testing_resource",
			Name: "another",
		}.Instance(addrs.IntKey(0)),
		&states.ResourceInstanceObjectSrc{
			Status: states.ObjectReady,
			AttrsJSON: []byte(`{
				"id": "foo",
				"value": "hello"
			}`),
		},
		mustDefaultRootProvider("testing"),
	)
	rootModule.SetResourceInstanceCurrent(
		addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "testing_resource",
			Name: "another",
		}.Instance(addrs.IntKey(1)),
		&states.ResourceInstanceObjectSrc{
			Status: states.ObjectReady,
			AttrsJSON: []byte(`{
				"id": "foo",
				"value": "hello"
			}`),
		},
		mustDefaultRootProvider("testing"),
	)

	childModule := state.EnsureModule(addrs.RootModuleInstance.Child("child_mod", addrs.NoKey))
	childModule.SetResourceInstanceCurrent(
		addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "testing_resource",
			Name: "child_data",
		}.Instance(addrs.NoKey),
		&states.ResourceInstanceObjectSrc{
			Status: states.ObjectReady,
			AttrsJSON: []byte(`{
				"id": "foo",
				"value": "hello"
			}`),
		},
		mustDefaultRootProvider("testing"),
	)
	childModule.SetResourceInstanceCurrent(
		addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "testing_resource",
			Name: "another_child_data",
		}.Instance(addrs.IntKey(0)),
		&states.ResourceInstanceObjectSrc{
			Status: states.ObjectReady,
			AttrsJSON: []byte(`{
				"id": "foo",
				"value": "hello"
			}`),
		},
		mustDefaultRootProvider("testing"),
	)
	childModule.SetResourceInstanceCurrent(
		addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "testing_resource",
			Name: "another_child_data",
		}.Instance(addrs.IntKey(1)),
		&states.ResourceInstanceObjectSrc{
			Status: states.ObjectReady,
			AttrsJSON: []byte(`{
				"id": "foo",
				"value": "hello"
			}`),
		},
		mustDefaultRootProvider("testing"),
	)

	mig := Migration{
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("testing"): func() (providers.Interface, error) {
				return stacks_testing_provider.NewProvider(t), nil
			},
		},
		PreviousState: state,
		Config:        cfg,
	}
	resources := map[string]string{
		"testing_resource.data":    "parent",
		"testing_resource.another": "parent",
	}
	modules := map[string]string{
		"child_mod": "child",
	}

	appliedResources := []*stackstate.AppliedChangeResourceInstanceObject{}
	appliedComponents := []*stackstate.AppliedChangeComponentInstance{}
	expectedResources := []*stackstate.AppliedChangeResourceInstanceObject{
		{
			ResourceInstanceObjectAddr: mustAbsResourceInstanceObject("component.parent.testing_resource.data"),
			NewStateSrc: &states.ResourceInstanceObjectSrc{
				AttrsJSON: mustMarshalJSONAttrs(map[string]interface{}{
					"id":    "foo",
					"value": "hello",
				}),
				Status:  states.ObjectReady,
				Private: nil,
			},
			ProviderConfigAddr: mustDefaultRootProvider("testing"),
			Schema:             stacks_testing_provider.TestingResourceSchema,
		},
		{
			ResourceInstanceObjectAddr: mustAbsResourceInstanceObject("component.parent.testing_resource.another[0]"),
			NewStateSrc: &states.ResourceInstanceObjectSrc{
				AttrsJSON: mustMarshalJSONAttrs(map[string]interface{}{
					"id":    "foo",
					"value": "hello",
				}),
				Status:  states.ObjectReady,
				Private: nil,
			},
			ProviderConfigAddr: mustDefaultRootProvider("testing"),
			Schema:             stacks_testing_provider.TestingResourceSchema,
		},
		{
			ResourceInstanceObjectAddr: mustAbsResourceInstanceObject("component.parent.testing_resource.another[1]"),
			NewStateSrc: &states.ResourceInstanceObjectSrc{
				AttrsJSON: mustMarshalJSONAttrs(map[string]interface{}{
					"id":    "foo",
					"value": "hello",
				}),
				Status:  states.ObjectReady,
				Private: nil,
			},
			ProviderConfigAddr: mustDefaultRootProvider("testing"),
			Schema:             stacks_testing_provider.TestingResourceSchema,
		},
		{
			ResourceInstanceObjectAddr: mustAbsResourceInstanceObject("component.child.testing_resource.child_data"),
			NewStateSrc: &states.ResourceInstanceObjectSrc{
				AttrsJSON: mustMarshalJSONAttrs(map[string]interface{}{
					"id":    "foo",
					"value": "hello",
				}),
				Status:  states.ObjectReady,
				Private: nil,
			},
			ProviderConfigAddr: mustDefaultRootProvider("testing"),
			Schema:             stacks_testing_provider.TestingResourceSchema,
		},
		{
			ResourceInstanceObjectAddr: mustAbsResourceInstanceObject("component.child.testing_resource.another_child_data[0]"),
			NewStateSrc: &states.ResourceInstanceObjectSrc{
				AttrsJSON: mustMarshalJSONAttrs(map[string]interface{}{
					"id":    "foo",
					"value": "hello",
				}),
				Status:  states.ObjectReady,
				Private: nil,
			},
			ProviderConfigAddr: mustDefaultRootProvider("testing"),
			Schema:             stacks_testing_provider.TestingResourceSchema,
		},
		{
			ResourceInstanceObjectAddr: mustAbsResourceInstanceObject("component.child.testing_resource.another_child_data[1]"),
			NewStateSrc: &states.ResourceInstanceObjectSrc{
				AttrsJSON: mustMarshalJSONAttrs(map[string]interface{}{
					"id":    "foo",
					"value": "hello",
				}),
				Status:  states.ObjectReady,
				Private: nil,
			},
			ProviderConfigAddr: mustDefaultRootProvider("testing"),
			Schema:             stacks_testing_provider.TestingResourceSchema,
		},
	}
	expectedComponents := []*stackstate.AppliedChangeComponentInstance{
		{
			ComponentAddr:         mustAbsComponent("component.parent"),
			ComponentInstanceAddr: mustAbsComponentInstance("component.parent"),
			OutputValues: map[addrs.OutputValue]cty.Value{
				{Name: "id"}: cty.DynamicVal,
			},
			InputVariables: map[addrs.InputVariable]cty.Value{
				{Name: "id"}:    cty.DynamicVal,
				{Name: "input"}: cty.DynamicVal,
			},
			Dependents: collections.NewSet(mustAbsComponent("component.child")),
		},
		{
			ComponentAddr:         mustAbsComponent("component.child"),
			ComponentInstanceAddr: mustAbsComponentInstance("component.child"),
			OutputValues: map[addrs.OutputValue]cty.Value{
				{Name: "id"}: cty.DynamicVal,
			},
			InputVariables: map[addrs.InputVariable]cty.Value{
				{Name: "id"}:    cty.DynamicVal,
				{Name: "input"}: cty.DynamicVal,
			},
			Dependencies: collections.NewSet(mustAbsComponent("component.parent")),
		},
	}

	var expDiags, gotDiags tfdiags.Diagnostics
	mig.Migrate(resources, modules, func(change stackstate.AppliedChange) {
		switch c := change.(type) {
		case *stackstate.AppliedChangeResourceInstanceObject:
			appliedResources = append(appliedResources, c)
		case *stackstate.AppliedChangeComponentInstance:
			appliedComponents = append(appliedComponents, c)
		}
	}, func(diagnostic tfdiags.Diagnostic) {
		gotDiags = append(gotDiags, diagnostic)
	})

	if diff := cmp.Diff(expDiags, gotDiags); diff != "" {
		t.Errorf("unexpected diagnostics:\n%s", diff)
	}

	if diff := compareAppliedChanges(t, expectedResources, appliedResources, func(c *stackstate.AppliedChangeResourceInstanceObject) string {
		return c.ResourceInstanceObjectAddr.String()
	}); diff != "" {
		t.Errorf("unexpected applied resource changes:\n%s", diff)
	}

	if diff := compareAppliedChanges(t, expectedComponents, appliedComponents, func(c *stackstate.AppliedChangeComponentInstance) string {
		return c.ComponentAddr.String()
	}); diff != "" {
		t.Errorf("unexpected applied component changes:\n%s", diff)
	}
}

func TestMigrateConfigDependsOn(t *testing.T) {
	cfg := loadMainBundleConfigForTest(t, filepath.Join("for-stacks-migrate", "with-depends-on"))

	lock := depsfile.NewLocks()
	lock.SetProvider(
		addrs.NewDefaultProvider("testing"),
		providerreqs.MustParseVersion("0.0.0"),
		providerreqs.MustParseVersionConstraints("=0.0.0"),
		providerreqs.PreferredHashes([]providerreqs.Hash{}),
	)

	state := states.BuildState(func(ss *states.SyncState) {
		ss.SetOutputValue(addrs.AbsOutputValue{
			Module:      addrs.RootModuleInstance,
			OutputValue: addrs.OutputValue{Name: "output"},
		}, cty.StringVal("before"), false)
	})

	rootModule := state.RootModule()
	rootModule.SetResourceInstanceCurrent(
		addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "testing_resource",
			Name: "data",
		}.Instance(addrs.NoKey),
		&states.ResourceInstanceObjectSrc{
			Status: states.ObjectReady,
			AttrsJSON: []byte(`{
				"id": "foo",
				"value": "depends_test"
			}`),
		},
		mustDefaultRootProvider("testing"),
	)
	rootModule.SetResourceInstanceCurrent(
		addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "testing_resource",
			Name: "second",
		}.Instance(addrs.NoKey),
		&states.ResourceInstanceObjectSrc{
			Status: states.ObjectReady,
			AttrsJSON: []byte(`{
				"id": "foo",
				"value": "depends_test"
			}`),
		},
		mustDefaultRootProvider("testing"),
	)
	rootModule.SetResourceInstanceCurrent(
		addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "testing_resource",
			Name: "third",
		}.Instance(addrs.NoKey),
		&states.ResourceInstanceObjectSrc{
			Status: states.ObjectReady,
			AttrsJSON: []byte(`{
				"id": "foo",
				"value": "depends_test"
			}`),
		},
		mustDefaultRootProvider("testing"),
	)

	mig := Migration{
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("testing"): func() (providers.Interface, error) {
				return stacks_testing_provider.NewProvider(t), nil
			},
		},
		PreviousState: state,
		Config:        cfg,
	}

	resources := map[string]string{
		"testing_resource.data":   "component.first",
		"testing_resource.second": "component.second",
		"testing_resource.third":  "stack.embedded.component.self",
	}
	modules := map[string]string{}

	appliedResources := []*stackstate.AppliedChangeResourceInstanceObject{}
	appliedComponents := []*stackstate.AppliedChangeComponentInstance{}
	expectedComponents := []*stackstate.AppliedChangeComponentInstance{
		{
			ComponentAddr:         mustAbsComponent("component.first"),
			ComponentInstanceAddr: mustAbsComponentInstance("component.first"),

			InputVariables: map[addrs.InputVariable]cty.Value{
				{Name: "input"}: cty.DynamicVal,
				{Name: "id"}:    cty.DynamicVal,
			},
			Dependents: collections.NewSet(mustAbsComponent("component.second")),
		},
		{
			ComponentAddr:         mustAbsComponent("component.second"),
			ComponentInstanceAddr: mustAbsComponentInstance("component.second"),
			InputVariables: map[addrs.InputVariable]cty.Value{
				{Name: "input"}: cty.DynamicVal,
				{Name: "id"}:    cty.DynamicVal,
			},
			Dependencies: collections.NewSet(mustAbsComponent("component.first"), mustAbsComponent("stack.embedded.component.self")),
		},
		{
			ComponentAddr:         mustAbsComponent("component.self"),
			ComponentInstanceAddr: mustAbsComponentInstance("stack.embedded.component.self"),
			InputVariables: map[addrs.InputVariable]cty.Value{
				{Name: "input"}: cty.DynamicVal,
				{Name: "id"}:    cty.DynamicVal,
			},
			Dependents: collections.NewSet(mustAbsComponent("component.second")),
		},
	}

	expectedResources := []*stackstate.AppliedChangeResourceInstanceObject{
		{
			ResourceInstanceObjectAddr: mustAbsResourceInstanceObject("component.first.testing_resource.data"),
			NewStateSrc: &states.ResourceInstanceObjectSrc{
				AttrsJSON: mustMarshalJSONAttrs(map[string]interface{}{
					"id":    "foo",
					"value": "depends_test",
				}),
				Status:  states.ObjectReady,
				Private: nil,
			},
			ProviderConfigAddr: mustDefaultRootProvider("testing"),
			Schema:             stacks_testing_provider.TestingResourceSchema,
		},
		{
			ResourceInstanceObjectAddr: mustAbsResourceInstanceObject("component.second.testing_resource.second"),
			NewStateSrc: &states.ResourceInstanceObjectSrc{
				AttrsJSON: mustMarshalJSONAttrs(map[string]interface{}{
					"id":    "foo",
					"value": "depends_test",
				}),
				Status:  states.ObjectReady,
				Private: nil,
			},
			ProviderConfigAddr: mustDefaultRootProvider("testing"),
			Schema:             stacks_testing_provider.TestingResourceSchema,
		},
		{
			ResourceInstanceObjectAddr: mustAbsResourceInstanceObject("stack.embedded.component.self.testing_resource.third"),
			NewStateSrc: &states.ResourceInstanceObjectSrc{
				AttrsJSON: mustMarshalJSONAttrs(map[string]interface{}{
					"id":    "foo",
					"value": "depends_test",
				}),
				Status:  states.ObjectReady,
				Private: nil,
			},
			ProviderConfigAddr: mustDefaultRootProvider("testing"),
			Schema:             stacks_testing_provider.TestingResourceSchema,
		},
	}

	var expDiags, gotDiags tfdiags.Diagnostics
	mig.Migrate(resources, modules, func(change stackstate.AppliedChange) {
		switch c := change.(type) {
		case *stackstate.AppliedChangeResourceInstanceObject:
			appliedResources = append(appliedResources, c)
		case *stackstate.AppliedChangeComponentInstance:
			appliedComponents = append(appliedComponents, c)
		}
	}, func(diagnostic tfdiags.Diagnostic) {
		gotDiags = append(gotDiags, diagnostic)
	})

	if diff := compareAppliedChanges(t, expectedComponents, appliedComponents, func(c *stackstate.AppliedChangeComponentInstance) string {
		return c.ComponentAddr.String()
	}); diff != "" {
		t.Fatalf("unexpected applied component changes:\n%s", diff)
	}

	if diff := compareAppliedChanges(t, expectedResources, appliedResources, func(c *stackstate.AppliedChangeResourceInstanceObject) string {
		return c.ResourceInstanceObjectAddr.String()
	}); diff != "" {
		t.Fatalf("unexpected applied resource changes:\n%s", diff)
	}

	if diff := cmp.Diff(expDiags, gotDiags); diff != "" {
		t.Fatalf("unexpected diagnostics:\n%s", diff)
	}
}

func compareAppliedChanges[A stackstate.AppliedChange, U stdcmp.Ordered](t *testing.T, expected, actual []A, cb func(A) U) string {
	t.Helper()

	if len(expected) != len(actual) {
		t.Fatalf("expected %d changes, got %d", len(expected), len(actual))
	}

	_exp := make([]U, len(expected))
	_act := make([]U, len(actual))
	mp_exp := make(map[U]A)
	mp_act := make(map[U]A)

	for i, exp := range expected {
		_exp[i] = cb(exp)
		_act[i] = cb(actual[i])
		mp_exp[_exp[i]] = exp
		mp_act[_act[i]] = actual[i]
	}

	sorter := cmpopts.SortMaps(func(a, b U) bool {
		return a < b
	})

	return cmp.Diff(mp_exp, mp_act, sorter, changesCmpOpts, cmpopts.EquateEmpty())
}

func TestMigrateConfig2(t *testing.T) {

	ctx := context.Background()
	cfg := loadMainBundleConfigForTest(t, filepath.Join("with-single-input", "valid"))

	fakePlanTimestamp, err := time.Parse(time.RFC3339, "2021-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}

	lock := depsfile.NewLocks()
	lock.SetProvider(
		addrs.NewDefaultProvider("testing"),
		providerreqs.MustParseVersion("0.0.0"),
		providerreqs.MustParseVersionConstraints("=0.0.0"),
		providerreqs.PreferredHashes([]providerreqs.Hash{}),
	)

	planReq := stackruntime.PlanRequest{
		PlanMode: plans.NormalMode,

		Config: cfg,
		ProviderFactories: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("testing"): func() (providers.Interface, error) {
				return stacks_testing_provider.NewProvider(t), nil
			},
		},
		DependencyLocks:    *lock,
		ForcePlanTimestamp: &fakePlanTimestamp,
		InputValues: map[stackaddrs.InputVariable]stackruntime.ExternalInputValue{
			{Name: "id"}: {
				Value: cty.StringVal("foo"),
			},
			{Name: "input"}: {
				Value: cty.StringVal("hello"),
			},
		},

		// We have in the previous state a resource that is not in our
		// underlying data store. This simulates the case where someone went
		// in and manually deleted a resource that Terraform is managing.
		//
		// Some providers will return an error in this case, but some will
		// not. We need to ensure that we handle the second case gracefully.
		// PrevState: stackstate.NewStateBuilder().
		// 	AddResourceInstance(stackstate.NewResourceInstanceBuilder().
		// 		SetAddr(mustAbsResourceInstanceObject("component.self.testing_resource.missing")).
		// 		SetProviderAddr(mustDefaultRootProvider("testing")).
		// 		SetResourceInstanceObjectSrc(states.ResourceInstanceObjectSrc{
		// 			SchemaVersion: 0,
		// 			AttrsJSON: mustMarshalJSONAttrs(map[string]interface{}{
		// 				"id":    "e84b59f2",
		// 				"value": "hello",
		// 			}),
		// 			Status: states.ObjectReady,
		// 		})).
		// 	Build(),
	}

	planChangesCh := make(chan stackplan.PlannedChange)
	planDiagsCh := make(chan tfdiags.Diagnostic)
	planResp := stackruntime.PlanResponse{
		PlannedChanges: planChangesCh,
		Diagnostics:    planDiagsCh,
	}

	go stackruntime.Plan(ctx, &planReq, &planResp)
	planChanges, planDiags := collectPlanOutput(planChangesCh, planDiagsCh)
	if len(planDiags) > 0 {
		t.Fatalf("unexpected diagnostics during planning: %s", planDiags)
	}

	planLoader := stackplan.NewLoader()
	for _, change := range planChanges {
		proto, err := change.PlannedChangeProto()
		if err != nil {
			t.Fatal(err)
		}

		for _, rawMsg := range proto.Raw {
			err = planLoader.AddRaw(rawMsg)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	plan, err := planLoader.Plan()
	if err != nil {
		t.Fatal(err)
	}

	state := states.BuildState(func(ss *states.SyncState) {
		ss.SetOutputValue(addrs.AbsOutputValue{
			Module:      addrs.RootModuleInstance,
			OutputValue: addrs.OutputValue{Name: "output"},
		}, cty.StringVal("before"), false)
	})
	rootModule := state.RootModule()
	rootModule.SetResourceInstanceCurrent(
		addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "testing_resource",
			Name: "data",
		}.Instance(addrs.NoKey),
		&states.ResourceInstanceObjectSrc{
			Status: states.ObjectReady,
			AttrsJSON: []byte(`{
				"id": "foo",
				"value": "hello"
			}`),
		},
		mustDefaultRootProvider("testing"),
	)
	// foo := states.NewResourceInstanceBuilder().
	// 	SetAddr(mustAbsResourceInstanceObject("component.self.testing_resource.missing")).
	// 	SetProviderAddr(mustDefaultRootProvider("testing")).
	// 	SetResourceInstanceObjectSrc(states.ResourceInstanceObjectSrc{
	// 		SchemaVersion: 0,
	// 		AttrsJSON: mustMarshalJSONAttrs(map[string]interface{}{
	// 			"id":    "e84b59f2",
	// 			"value": "hello",
	// 		}),
	// 		Status: states.ObjectReady,
	// 	})
	_ = plan
	mig := Migration{
		Providers:     planReq.ProviderFactories,
		PreviousState: state,
		Config:        cfg,
	}
	resources := map[string]string{
		"testing_resource.data": "self",
	}
	applied := []stackstate.AppliedChange{}
	expected := []stackstate.AppliedChange{
		&stackstate.AppliedChangeResourceInstanceObject{
			ResourceInstanceObjectAddr: mustAbsResourceInstanceObject("component.self.testing_resource.data"),
			NewStateSrc: &states.ResourceInstanceObjectSrc{
				AttrsJSON: mustMarshalJSONAttrs(map[string]interface{}{
					"id":    "foo",
					"value": "hello",
				}),
				Status:  states.ObjectReady,
				Private: nil,
			},
			ProviderConfigAddr: mustDefaultRootProvider("testing"),
			Schema:             stacks_testing_provider.TestingResourceSchema,
		},
		&stackstate.AppliedChangeComponentInstance{
			ComponentAddr:         mustAbsComponent("component.self"),
			ComponentInstanceAddr: mustAbsComponentInstance("component.self"),
			OutputValues:          map[addrs.OutputValue]cty.Value{},
			InputVariables: map[addrs.InputVariable]cty.Value{
				{Name: "id"}:    cty.DynamicVal,
				{Name: "input"}: cty.DynamicVal,
			},
		},
	}
	modules := map[string]string{}
	mig.Migrate(resources, modules, func(change stackstate.AppliedChange) {
		applied = append(applied, change)
	}, func(diagnostic tfdiags.Diagnostic) {
		fmt.Println(diagnostic)
	})

	if diff := cmp.Diff(expected, applied, changesCmpOpts); diff != "" {
		t.Fatalf("unexpected applied changes:\n%s", diff)
	}

	fmt.Println(applied)
}

func cmpJSONMap() cmp.Option {
	return cmp.FilterValues(func(x, y interface{}) bool {
		_, okX := x.([]uint8)
		_, okY := y.([]uint8)
		return okX && okY
	}, cmp.Comparer(func(x, y interface{}) bool {
		var xJSON, yJSON map[string]interface{}
		err := json.Unmarshal(x.([]uint8), &xJSON)
		if err != nil {
			return false
		}
		err = json.Unmarshal(y.([]uint8), &yJSON)
		if err != nil {
			return false
		}

		return cmp.Equal(xJSON, yJSON)
	}))
}

var changesCmpOpts = cmp.Options{
	ctydebug.CmpOptions,
	cmpCollectionsSet,
	cmpopts.IgnoreUnexported(addrs.InputVariable{}),
	cmpopts.IgnoreUnexported(states.ResourceInstanceObjectSrc{}),
	cmpJSONMap(),
	cmpopts.IgnoreFields(states.ResourceInstanceObjectSrc{}, "Private"),
}

var cmpCollectionsSet = cmp.Comparer(func(x, y collections.Set[stackaddrs.AbsComponent]) bool {
	if x.Len() != y.Len() {
		return false
	}

	for v := range x.All() {
		if !y.Has(v) {
			return false
		}
	}

	return true
})

func TestMigrateConfigWithNoChanges(t *testing.T) {

}

// collectPlanOutput consumes the two output channels emitting results from
// a call to [Plan], and collects all of the data written to them before
// returning once changesCh has been closed by the sender to indicate that
// the planning process is complete.
func collectPlanOutput(changesCh <-chan stackplan.PlannedChange, diagsCh <-chan tfdiags.Diagnostic) ([]stackplan.PlannedChange, tfdiags.Diagnostics) {
	var changes []stackplan.PlannedChange
	var diags tfdiags.Diagnostics

	for {
		select {
		case change, ok := <-changesCh:
			if !ok {
				// The plan operation is complete but we might still have
				// some buffered diagnostics to consume.
				if diagsCh != nil {
					for diag := range diagsCh {
						diags = append(diags, diag)
					}
				}
				return changes, diags
			}
			changes = append(changes, change)
		case diag, ok := <-diagsCh:
			if !ok {
				// no more diagnostics to read
				diagsCh = nil
				continue
			}
			diags = append(diags, diag)
		}
	}
}

func mustMarshalJSONAttrs(attrs map[string]interface{}) []byte {
	jsonAttrs, err := json.Marshal(attrs)
	if err != nil {
		panic(err)
	}
	return jsonAttrs
}

func mustDefaultRootProvider(provider string) addrs.AbsProviderConfig {
	return addrs.AbsProviderConfig{
		Module:   addrs.RootModule,
		Provider: addrs.NewDefaultProvider(provider),
	}
}

func mustAbsResourceInstance(addr string) addrs.AbsResourceInstance {
	ret, diags := addrs.ParseAbsResourceInstanceStr(addr)
	if len(diags) > 0 {
		panic(fmt.Sprintf("failed to parse resource instance address %q: %s", addr, diags))
	}
	return ret
}

func mustAbsResourceInstanceObject(addr string) stackaddrs.AbsResourceInstanceObject {
	ret, diags := stackaddrs.ParseAbsResourceInstanceObjectStr(addr)
	if len(diags) > 0 {
		panic(fmt.Sprintf("failed to parse resource instance object address %q: %s", addr, diags))
	}
	return ret
}

func mustAbsResourceInstanceObjectPtr(addr string) *stackaddrs.AbsResourceInstanceObject {
	ret := mustAbsResourceInstanceObject(addr)
	return &ret
}

func mustAbsComponentInstance(addr string) stackaddrs.AbsComponentInstance {
	ret, diags := stackaddrs.ParsePartialComponentInstanceStr(addr)
	if len(diags) > 0 {
		panic(fmt.Sprintf("failed to parse component instance address %q: %s", addr, diags))
	}
	return ret
}

func mustAbsComponent(addr string) stackaddrs.AbsComponent {
	ret, diags := stackaddrs.ParsePartialComponentInstanceStr(addr)
	if len(diags) > 0 {
		panic(fmt.Sprintf("failed to parse component instance address %q: %s", addr, diags))
	}
	return stackaddrs.AbsComponent{
		Stack: ret.Stack,
		Item:  ret.Item.Component,
	}
}

// TODO: Perhaps export this from helper_test instead
func loadMainBundleConfigForTest(t *testing.T, dirName string) *stackconfig.Config {
	t.Helper()
	fullSourceAddr := mainBundleSourceAddrStr(dirName)
	return loadConfigForTest(t, "../stackruntime/testdata/mainbundle", fullSourceAddr)
}

func mainBundleSourceAddrStr(dirName string) string {
	return "git::https://example.com/test.git//" + dirName
}

// loadConfigForTest is a test helper that tries to open bundleRoot as a
// source bundle, and then if successful tries to load the given source address
// from it as a stack configuration. If any part of the operation fails then
// it halts execution of the test and doesn't return.
func loadConfigForTest(t *testing.T, bundleRoot string, configSourceAddr string) *stackconfig.Config {
	t.Helper()
	sources, err := sourcebundle.OpenDir(bundleRoot)
	if err != nil {
		t.Fatalf("cannot load source bundle: %s", err)
	}

	// We force using remote source addresses here because that avoids
	// us having to deal with the extra version constraints argument
	// that registry sources require. Exactly what source address type
	// we use isn't relevant for tests in this package, since it's
	// the sourcebundle package's responsibility to make sure its
	// abstraction works for all of the source types.
	sourceAddr, err := sourceaddrs.ParseRemoteSource(configSourceAddr)
	if err != nil {
		t.Fatalf("invalid config source address: %s", err)
	}

	cfg, diags := stackconfig.LoadConfigDir(sourceAddr, sources)
	reportDiagnosticsForTest(t, diags)
	return cfg
}

// reportDiagnosticsForTest creates a test log entry for every diagnostic in
// the given diags, and halts the test if any of them are error diagnostics.
func reportDiagnosticsForTest(t *testing.T, diags tfdiags.Diagnostics) {
	t.Helper()
	for _, diag := range diags {
		var b strings.Builder
		desc := diag.Description()
		locs := diag.Source()

		switch sev := diag.Severity(); sev {
		case tfdiags.Error:
			b.WriteString("Error: ")
		case tfdiags.Warning:
			b.WriteString("Warning: ")
		default:
			t.Errorf("unsupported diagnostic type %s", sev)
		}
		b.WriteString(desc.Summary)
		if desc.Address != "" {
			b.WriteString("\nwith ")
			b.WriteString(desc.Summary)
		}
		if locs.Subject != nil {
			b.WriteString("\nat ")
			b.WriteString(locs.Subject.StartString())
		}
		if desc.Detail != "" {
			b.WriteString("\n\n")
			b.WriteString(desc.Detail)
		}
		t.Log(b.String())
	}
	if diags.HasErrors() {
		t.FailNow()
	}
}
