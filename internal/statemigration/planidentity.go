package statemigration

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

type planInstanceIdentity struct {
	Module string
	Mode   string
	Type   string
	Name   string
	Index  json.RawMessage
}

func parsePlanIdentity(address string) (planInstanceIdentity, error) {
	identity := planInstanceIdentity{Module: "", Mode: "managed", Type: "", Name: "", Index: nil}
	traversal, diagnostics := hclsyntax.ParseTraversalAbs([]byte(address), "migration instance identity", hcl.InitialPos)
	if diagnostics.HasErrors() {
		return identity, errors.New("the plan contains an invalid native instance identity")
	}
	module, position, err := parseModulePrefix(traversal)
	if err != nil {
		return identity, err
	}
	identity.Module = module
	if position < len(traversal) && traversalWord(traversal[position]) == "data" {
		identity.Mode = "data"
		position++
	}
	if position+2 > len(traversal) {
		return identity, errors.New("the plan contains an incomplete resource instance identity")
	}
	identity.Type, identity.Name = traversalWord(traversal[position]), traversalWord(traversal[position+1])
	position += 2
	if identity.Type == "" || identity.Name == "" {
		return identity, errors.New("the plan contains an unsupported resource instance identity")
	}
	if position == len(traversal) {
		return identity, nil
	}
	index, indexed := traversal[position].(hcl.TraverseIndex)
	if !indexed || position+1 != len(traversal) {
		return identity, errors.New("the plan contains an unsupported resource instance index")
	}
	encoded, err := ctyjson.Marshal(index.Key, index.Key.Type())
	if err != nil {
		return identity, errors.New("encode the reviewed native instance index")
	}
	identity.Index = encoded
	return identity, nil
}

func parseModulePrefix(traversal hcl.Traversal) (string, int, error) {
	position := 0
	for position < len(traversal) && traversalWord(traversal[position]) == "module" {
		position += 2
		if position > len(traversal) {
			return "", 0, errors.New("the plan contains an incomplete module instance identity")
		}
		if position < len(traversal) {
			if _, indexed := traversal[position].(hcl.TraverseIndex); indexed {
				position++
			}
		}
	}
	if position == 0 {
		return "", position, nil
	}
	return strings.TrimSpace(string(hclwrite.TokensForTraversal(traversal[:position]).Bytes())), position, nil
}

func traversalWord(traverser hcl.Traverser) string {
	switch traverser := traverser.(type) {
	case hcl.TraverseRoot:
		return traverser.Name
	case hcl.TraverseAttr:
		return traverser.Name
	default:
		return ""
	}
}

func normalizePlanIdentity(fields map[string]json.RawMessage, address string) error {
	identity, err := parsePlanIdentity(address)
	if err != nil {
		return err
	}
	var resourceType, mode string
	if json.Unmarshal(fields["type"], &resourceType) != nil || json.Unmarshal(fields["mode"], &mode) != nil || resourceType != identity.Type || mode != identity.Mode {
		return errors.New("the reviewed instance mapping changes the plan resource type or mode")
	}
	for key, value := range map[string]string{"address": address, "name": identity.Name, "module_address": identity.Module} {
		if key == "module_address" && value == "" {
			delete(fields, key)
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return errors.New("encode the normalized native plan instance identity")
		}
		fields[key] = encoded
	}
	if identity.Index == nil {
		delete(fields, "index")
	} else {
		fields["index"] = identity.Index
	}
	return nil
}
