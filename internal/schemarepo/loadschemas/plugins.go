// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package loadschemas

import (
	"fmt"
	"log"

	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/configs/configschema"
	"github.com/hashicorp/terraform/internal/providers"
	"github.com/hashicorp/terraform/internal/provisioners"
)

// Plugins represents a library of available plugins for which it's safe
// to cache certain information for performance reasons.
type Plugins struct {
	providerFactories    map[addrs.Provider]providers.Factory
	provisionerFactories map[string]provisioners.Factory

	preloadedProviderSchemas         map[addrs.Provider]providers.ProviderSchema
	preloadedResourceIdentitySchemas map[addrs.Provider]providers.ResourceIdentitySchemas
}

func NewPlugins(
	providerFactories map[addrs.Provider]providers.Factory,
	provisionerFactories map[string]provisioners.Factory,
	preloadedProviderSchemas map[addrs.Provider]providers.ProviderSchema,
	preloadedResourceIdentitySchemas map[addrs.Provider]providers.ResourceIdentitySchemas,
) *Plugins {
	ret := &Plugins{
		providerFactories:                providerFactories,
		provisionerFactories:             provisionerFactories,
		preloadedProviderSchemas:         preloadedProviderSchemas,
		preloadedResourceIdentitySchemas: preloadedResourceIdentitySchemas,
	}
	return ret
}

// ProviderFactories returns a map of all of the registered provider factories.
//
// Callers must not modify the returned map and must not access it concurrently
// with any other method of this type.
func (cp *Plugins) ProviderFactories() map[addrs.Provider]providers.Factory {
	return cp.providerFactories
}

func (cp *Plugins) HasProvider(addr addrs.Provider) bool {
	_, ok := cp.providerFactories[addr]
	return ok
}

func (cp *Plugins) HasPreloadedSchemaForProvider(addr addrs.Provider) bool {
	_, ok := cp.preloadedProviderSchemas[addr]
	return ok
}

func (cp *Plugins) HasPreloadedResourceIdentitySchemasForProvider(addr addrs.Provider) bool {
	_, ok := cp.preloadedResourceIdentitySchemas[addr]
	return ok
}

func (cp *Plugins) NewProviderInstance(addr addrs.Provider) (providers.Interface, error) {
	f, ok := cp.providerFactories[addr]
	if !ok {
		return nil, fmt.Errorf("unavailable provider %q", addr.String())
	}

	return f()
}

// ProvisionerFactories returns a map of all of the registered provisioner
// factories.
//
// Callers must not modify the returned map and must not access it concurrently
// with any other method of this type.
func (cp *Plugins) ProvisionerFactories() map[string]provisioners.Factory {
	return cp.provisionerFactories
}

func (cp *Plugins) HasProvisioner(typ string) bool {
	_, ok := cp.provisionerFactories[typ]
	return ok
}

func (cp *Plugins) NewProvisionerInstance(typ string) (provisioners.Interface, error) {
	f, ok := cp.provisionerFactories[typ]
	if !ok {
		return nil, fmt.Errorf("unavailable provisioner %q", typ)
	}

	return f()
}

// ProviderSchema uses a temporary instance of the provider with the given
// address to obtain the full schema for all aspects of that provider.
//
// ProviderSchema memoizes results by unique provider address, so it's fine
// to repeatedly call this method with the same address if various different
// parts of Terraform all need the same schema information.
func (cp *Plugins) ProviderSchema(addr addrs.Provider) (providers.ProviderSchema, error) {
	// Check the global schema cache first.
	// This cache is only written by the provider client, and transparently
	// used by GetProviderSchema, but we check it here because at this point we
	// may be able to avoid spinning up the provider instance at all.
	// We skip this if we have preloaded schemas because that suggests that
	// our caller is not Terraform CLI and therefore it's probably inappropriate
	// to assume that provider schemas are unique process-wide.
	//
	// FIXME: A global cache is inappropriate when Terraform Core is being
	// used in a non-Terraform-CLI mode where we shouldn't assume that all
	// calls share the same provider implementations.
	schemas, ok := providers.SchemaCache.Get(addr)
	if ok {
		log.Printf("[TRACE] terraform.contextPlugins: Schema for provider %q is in the global cache", addr)
		return schemas, nil
	}

	// We might have a non-global preloaded copy of this provider's schema.
	if schema, ok := cp.preloadedProviderSchemas[addr]; ok {
		log.Printf("[TRACE] terraform.contextPlugins: Provider %q has a preloaded schema", addr)
		return schema, nil
	}

	log.Printf("[TRACE] terraform.contextPlugins: Initializing provider %q to read its schema", addr)
	provider, err := cp.NewProviderInstance(addr)
	if err != nil {
		return schemas, fmt.Errorf("failed to instantiate provider %q to obtain schema: %s", addr, err)
	}
	defer provider.Close()

	resp := provider.GetProviderSchema()
	if resp.Diagnostics.HasErrors() {
		return resp, fmt.Errorf("failed to retrieve schema from provider %q: %s", addr, resp.Diagnostics.Err())
	}

	if resp.Provider.Version < 0 {
		// We're not using the version numbers here yet, but we'll check
		// for validity anyway in case we start using them in future.
		return resp, fmt.Errorf("provider %s has invalid negative schema version for its configuration blocks,which is a bug in the provider ", addr)
	}

	for t, r := range resp.ResourceTypes {
		if err := r.Body.InternalValidate(); err != nil {
			return resp, fmt.Errorf("provider %s has invalid schema for managed resource type %q, which is a bug in the provider: %q", addr, t, err)
		}
		if r.Version < 0 {
			return resp, fmt.Errorf("provider %s has invalid negative schema version for managed resource type %q, which is a bug in the provider", addr, t)
		}
	}

	for t, d := range resp.DataSources {
		if err := d.Body.InternalValidate(); err != nil {
			return resp, fmt.Errorf("provider %s has invalid schema for data resource type %q, which is a bug in the provider: %q", addr, t, err)
		}
		if d.Version < 0 {
			// We're not using the version numbers here yet, but we'll check
			// for validity anyway in case we start using them in future.
			return resp, fmt.Errorf("provider %s has invalid negative schema version for data resource type %q, which is a bug in the provider", addr, t)
		}
	}

	for t, r := range resp.EphemeralResourceTypes {
		if err := r.Body.InternalValidate(); err != nil {
			return resp, fmt.Errorf("provider %s has invalid schema for ephemeral resource type %q, which is a bug in the provider: %q", addr, t, err)
		}
	}

	for n, f := range resp.Functions {
		if !hclsyntax.ValidIdentifier(n) {
			return resp, fmt.Errorf("provider %s declares function with invalid name %q", addr, n)
		}
		// We'll also do some enforcement of parameter names, even though they
		// are only for docs/UI for now, to leave room for us to potentially
		// use them for other purposes later.
		seenParams := make(map[string]int, len(f.Parameters))
		for i, p := range f.Parameters {
			if !hclsyntax.ValidIdentifier(p.Name) {
				return resp, fmt.Errorf("provider %s function %q declares invalid name %q for parameter %d", addr, n, p.Name, i)
			}
			if prevIdx, exists := seenParams[p.Name]; exists {
				return resp, fmt.Errorf("provider %s function %q reuses name %q for both parameters %d and %d", addr, n, p.Name, prevIdx, i)
			}
			seenParams[p.Name] = i
		}
		if p := f.VariadicParameter; p != nil {
			if !hclsyntax.ValidIdentifier(p.Name) {
				return resp, fmt.Errorf("provider %s function %q declares invalid name %q for its variadic parameter", addr, n, p.Name)
			}
			if prevIdx, exists := seenParams[p.Name]; exists {
				return resp, fmt.Errorf("provider %s function %q reuses name %q for both parameter %d and its variadic parameter", addr, n, p.Name, prevIdx)
			}
		}
	}

	return resp, nil
}

func (cp *Plugins) ResourceIdentitySchemas(addr addrs.Provider) (providers.ResourceIdentitySchemas, error) {
	// Check the global schema cache first.
	// This cache is only written by the provider client, and transparently
	// used by GetProviderSchema, but we check it here because at this point we
	// may be able to avoid spinning up the provider instance at all.
	// We skip this if we have preloaded schemas because that suggests that
	// our caller is not Terraform CLI and therefore it's probably inappropriate
	// to assume that provider schemas are unique process-wide.
	//
	// FIXME: A global cache is inappropriate when Terraform Core is being
	// used in a non-Terraform-CLI mode where we shouldn't assume that all
	// calls share the same provider implementations.
	schemas, ok := providers.ResourceIdentitySchemasCache.Get(addr)
	if ok {
		log.Printf("[TRACE] terraform.contextPlugins: Resource identity schemas for provider %q is in the global cache", addr)
		return schemas, nil
	}

	if is, ok := cp.preloadedResourceIdentitySchemas[addr]; ok {
		return is, nil
	}

	log.Printf("[TRACE] terraform.contextPlugins: Initializing provider %q to read its resource identity schemas", addr)
	provider, err := cp.NewProviderInstance(addr)
	if err != nil {
		// A provider might have provided preloaded resource schemas but is missing the identity schemas.
		// We will return an empty set of schemas in this case.
		log.Printf("[TRACE] failed to instantiate provider %q to obtain resource identity schemas: %s", addr, err)
		return providers.ResourceIdentitySchemas{}, nil
	}
	defer provider.Close()

	resp := provider.GetResourceIdentitySchemas()
	if resp.Diagnostics.HasErrors() {
		for _, diag := range resp.Diagnostics {
			if diag.Description().Summary == "Unsupported plugin method" {
				// Some providers may not support the GetResourceIdentitySchemas method.
				// In this case, we return an empty set of schemas.
				return resp, nil
			}
		}

		return resp, fmt.Errorf("failed to retrieve resource identity schemas from provider %q: %s", addr, resp.Diagnostics.Err())
	}

	for t, r := range resp.IdentityTypes {
		if r.Version < 0 {
			return resp, fmt.Errorf("provider %s has invalid negative schema version for managed resource type %q, which is a bug in the provider", addr, t)
		}

		for attrName, attrTy := range r.Attributes.ImpliedType().AttributeTypes() {
			if attrTy.MapElementType() != nil {
				return resp, fmt.Errorf("provider %s has invalid schema for managed resource type %q, attribute %q is a map, which is not allowed in identity schemas", addr, t, attrName)
			}

			if attrTy.SetElementType() != nil {
				return resp, fmt.Errorf("provider %s has invalid schema for managed resource type %q, attribute %q is a set, which is not allowed in identity schemas", addr, t, attrName)
			}

			if attrTy.IsObjectType() {
				return resp, fmt.Errorf("provider %s has invalid schema for managed resource type %q, attribute %q is an object, which is not allowed in identity schemas", addr, t, attrName)
			}
		}
	}

	return resp, nil
}

// ProviderConfigSchema is a helper wrapper around ProviderSchema which first
// reads the full schema of the given provider and then extracts just the
// provider's configuration schema, which defines what's expected in a
// "provider" block in the configuration when configuring this provider.
func (cp *Plugins) ProviderConfigSchema(providerAddr addrs.Provider) (*configschema.Block, error) {
	providerSchema, err := cp.ProviderSchema(providerAddr)
	if err != nil {
		return nil, err
	}

	return providerSchema.Provider.Body, nil
}

// ResourceTypeSchema is a helper wrapper around ProviderSchema which first
// reads the schema of the given provider and then tries to find the schema
// for the resource type of the given resource mode in that provider.
//
// ResourceTypeSchema will return an error if the provider schema lookup
// fails, but will return nil if the provider schema lookup succeeds but then
// the provider doesn't have a resource of the requested type.
//
// Managed resource types have versioned schemas, so the second return value
// is the current schema version number for the requested resource. The version
// is irrelevant for other resource modes.
func (cp *Plugins) ResourceTypeSchema(providerAddr addrs.Provider, resourceMode addrs.ResourceMode, resourceType string) (*configschema.Block, uint64, error) {
	providerSchema, err := cp.ProviderSchema(providerAddr)
	if err != nil {
		return nil, 0, err
	}

	schema, version := providerSchema.SchemaForResourceType(resourceMode, resourceType)
	return schema, version, nil
}

// ProvisionerSchema uses a temporary instance of the provisioner with the
// given type name to obtain the schema for that provisioner's configuration.
//
// ProvisionerSchema memoizes results by provisioner type name, so it's fine
// to repeatedly call this method with the same name if various different
// parts of Terraform all need the same schema information.
func (cp *Plugins) ProvisionerSchema(typ string) (*configschema.Block, error) {
	log.Printf("[TRACE] terraform.contextPlugins: Initializing provisioner %q to read its schema", typ)
	provisioner, err := cp.NewProvisionerInstance(typ)
	if err != nil {
		return nil, fmt.Errorf("failed to instantiate provisioner %q to obtain schema: %s", typ, err)
	}
	defer provisioner.Close()

	resp := provisioner.GetSchema()
	if resp.Diagnostics.HasErrors() {
		return nil, fmt.Errorf("failed to retrieve schema from provisioner %q: %s", typ, resp.Diagnostics.Err())
	}

	return resp.Provisioner, nil
}

// ProviderFunctionDecls is a helper wrapper around ProviderSchema which first
// reads the schema of the given provider and then returns all of the
// functions it declares, if any.
//
// ProviderFunctionDecl will return an error if the provider schema lookup
// fails, but will return an empty set of functions if a successful response
// returns no functions, or if the provider is using an older protocol version
// which has no support for provider-contributed functions.
func (cp *Plugins) ProviderFunctionDecls(providerAddr addrs.Provider) (map[string]providers.FunctionDecl, error) {
	providerSchema, err := cp.ProviderSchema(providerAddr)
	if err != nil {
		return nil, err
	}

	return providerSchema.Functions, nil
}
