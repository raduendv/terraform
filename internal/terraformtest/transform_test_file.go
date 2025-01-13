// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package terraformtest

import (
	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/configs"
	"github.com/hashicorp/terraform/internal/moduletest"
	"github.com/hashicorp/terraform/internal/terraform"
)

// FileVariablesTransformer is a GraphTransformer that adds the file top-level variables to the graph.
type FileVariablesTransformer struct {
	File   *moduletest.File
	config *configs.Config
}

func (t *FileVariablesTransformer) Transform(g *terraform.Graph) error {
	// add the file top-level variables
	for name, expr := range t.File.Config.Variables {
		node := &nodeFileVariable{
			Addr:   addrs.InputVariable{Name: name},
			Expr:   expr,
			config: t.config,
			Module: t.config.Path,
		}
		g.Add(node)
	}
	return nil
}
