package earlydecoder

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/terraform-schema/module"
)

// decodedModule is the type representing a decoded Terraform module.
type decodedModule struct {
	RequiredCore         []string
	ProviderRequirements map[string]*providerRequirement
	ProviderConfigs      map[string]*providerConfig
	Resources            map[string]*resource
	DataSources          map[string]*dataSource
	ModuleSources        map[string]*module.ModuleSource
}

func newDecodedModule() *decodedModule {
	return &decodedModule{
		RequiredCore:         make([]string, 0),
		ProviderRequirements: make(map[string]*providerRequirement, 0),
		ProviderConfigs:      make(map[string]*providerConfig, 0),
		Resources:            make(map[string]*resource, 0),
		DataSources:          make(map[string]*dataSource, 0),
		ModuleSources:        make(map[string]*module.ModuleSource, 0),
	}
}

// providerConfig represents a provider block in the configuration
type providerConfig struct {
	Name  string
	Alias string
}

// loadModuleFromFile reads given file, interprets it and stores in given Module
// This is useful for any caller which does tokenization/parsing on its own
// e.g. because it will reuse these parsed files later for more detailed
// interpretation.
func loadModuleFromFile(file *hcl.File, mod *decodedModule) hcl.Diagnostics {
	var diags hcl.Diagnostics
	content, _, contentDiags := file.Body.PartialContent(rootSchema)
	diags = append(diags, contentDiags...)

	for _, block := range content.Blocks {
		switch block.Type {

		case "terraform":
			content, _, contentDiags := block.Body.PartialContent(terraformBlockSchema)
			diags = append(diags, contentDiags...)

			if attr, defined := content.Attributes["required_version"]; defined {
				var version string
				valDiags := gohcl.DecodeExpression(attr.Expr, nil, &version)
				diags = append(diags, valDiags...)
				if !valDiags.HasErrors() {
					mod.RequiredCore = append(mod.RequiredCore, version)
				}
			}

			for _, innerBlock := range content.Blocks {
				switch innerBlock.Type {
				case "required_providers":
					reqs, reqsDiags := decodeRequiredProvidersBlock(innerBlock)
					diags = append(diags, reqsDiags...)
					for name, req := range reqs {
						if _, exists := mod.ProviderRequirements[name]; !exists {
							mod.ProviderRequirements[name] = req
						} else {
							if req.Source != "" {
								source := mod.ProviderRequirements[name].Source
								if source != "" && source != req.Source {
									diags = append(diags, &hcl.Diagnostic{
										Severity: hcl.DiagError,
										Summary:  "Multiple provider source attributes",
										Detail:   fmt.Sprintf("Found multiple source attributes for provider %s: %q, %q", name, source, req.Source),
										Subject:  &innerBlock.DefRange,
									})
								} else {
									mod.ProviderRequirements[name].Source = req.Source
								}
							}

							mod.ProviderRequirements[name].VersionConstraints = append(mod.ProviderRequirements[name].VersionConstraints, req.VersionConstraints...)
						}
					}
				}
			}
		case "provider":
			content, _, contentDiags := block.Body.PartialContent(providerConfigSchema)
			diags = append(diags, contentDiags...)

			name := block.Labels[0]
			// Even if there isn't an explicit version required, we still
			// need an entry in our map to signal the unversioned dependency.
			if _, exists := mod.ProviderRequirements[name]; !exists {
				mod.ProviderRequirements[name] = &providerRequirement{}
			}
			if attr, defined := content.Attributes["version"]; defined {
				var version string
				valDiags := gohcl.DecodeExpression(attr.Expr, nil, &version)
				diags = append(diags, valDiags...)
				if !valDiags.HasErrors() {
					mod.ProviderRequirements[name].VersionConstraints = append(mod.ProviderRequirements[name].VersionConstraints, version)
				}
			}

			providerKey := name
			var alias string
			if attr, defined := content.Attributes["alias"]; defined {
				valDiags := gohcl.DecodeExpression(attr.Expr, nil, &alias)
				diags = append(diags, valDiags...)
				if !valDiags.HasErrors() && alias != "" {
					providerKey = fmt.Sprintf("%s.%s", name, alias)
				}
			}

			mod.ProviderConfigs[providerKey] = &providerConfig{
				Name:  name,
				Alias: alias,
			}

		case "data":
			content, _, contentDiags := block.Body.PartialContent(resourceSchema)
			diags = append(diags, contentDiags...)

			ds := &dataSource{
				Type: block.Labels[0],
				Name: block.Labels[1],
			}

			mod.DataSources[ds.MapKey()] = ds

			if attr, defined := content.Attributes["provider"]; defined {
				ref, aDiags := decodeProviderAttribute(attr)
				diags = append(diags, aDiags...)
				ds.Provider = ref
			} else {
				// If provider _isn't_ set then we'll infer it from the
				// datasource type.
				ds.Provider = module.ProviderRef{
					LocalName: inferProviderNameFromType(ds.Type),
				}
			}

		case "resource":
			content, _, contentDiags := block.Body.PartialContent(resourceSchema)
			diags = append(diags, contentDiags...)

			r := &resource{
				Type: block.Labels[0],
				Name: block.Labels[1],
			}

			mod.Resources[r.MapKey()] = r

			if attr, defined := content.Attributes["provider"]; defined {
				ref, aDiags := decodeProviderAttribute(attr)
				diags = append(diags, aDiags...)
				r.Provider = ref
			} else {
				// If provider _isn't_ set then we'll infer it from the
				// resource type.
				r.Provider = module.ProviderRef{
					LocalName: inferProviderNameFromType(r.Type),
				}
			}
		case "module":
			content, _, contentDiags := block.Body.PartialContent(moduleSchema)
			diags = append(diags, contentDiags...)

			ms := &module.ModuleSource{Name: block.Labels[0]}
			mod.ModuleSources[ms.MapKey()] = ms

			if attr, defined := content.Attributes["source"]; defined {
				// decodeModuleAttribute
				valDiags := gohcl.DecodeExpression(attr.Expr, nil, &ms.Source)
				diags = append(diags, valDiags...)
			}
		}
	}

	return diags
}

func decodeProviderAttribute(attr *hcl.Attribute) (module.ProviderRef, hcl.Diagnostics) {
	var diags hcl.Diagnostics

	// New style here is to provide this as a naked traversal
	// expression, but we also support quoted references for
	// older configurations that predated this convention.
	traversal, travDiags := hcl.AbsTraversalForExpr(attr.Expr)
	if travDiags.HasErrors() {
		traversal = nil // in case we got any partial results

		// Fall back on trying to parse as a string
		var travStr string
		valDiags := gohcl.DecodeExpression(attr.Expr, nil, &travStr)
		if !valDiags.HasErrors() {
			var strDiags hcl.Diagnostics
			traversal, strDiags = hclsyntax.ParseTraversalAbs([]byte(travStr), "", hcl.Pos{})
			if strDiags.HasErrors() {
				traversal = nil
			}
		}
	}

	// If we get out here with a nil traversal then we didn't
	// succeed in processing the input.
	if len(traversal) > 0 {
		providerName := traversal.RootName()
		alias := ""
		if len(traversal) > 1 {
			if getAttr, ok := traversal[1].(hcl.TraverseAttr); ok {
				alias = getAttr.Name
			}
		}
		return module.ProviderRef{
			LocalName: providerName,
			Alias:     alias,
		}, diags
	}

	return module.ProviderRef{}, hcl.Diagnostics{
		&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Invalid provider reference",
			Detail:   "Provider argument requires a provider name followed by an optional alias, like \"aws.foo\".",
			Subject:  attr.Expr.Range().Ptr(),
		},
	}
}
