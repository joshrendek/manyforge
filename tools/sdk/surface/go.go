// Inspect compiler export data, including instantiated generic public APIs.
package main

import (
	"encoding/json"
	"fmt"
	"go/importer"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
)

type node = map[string]any

func typeName(t types.Type) string {
	switch value := t.(type) {
	case *types.Signature:
		params, results := []string{}, []string{}
		for i := range value.Params().Len() {
			parameter := typeName(value.Params().At(i).Type())
			if value.Variadic() && i == value.Params().Len()-1 {
				parameter = "..." + strings.TrimPrefix(parameter, "[]")
			}
			params = append(params, parameter)
		}
		for i := range value.Results().Len() {
			results = append(results, typeName(value.Results().At(i).Type()))
		}
		return "func(" + strings.Join(params, ", ") + ") (" + strings.Join(results, ", ") + ")"
	case *types.Pointer:
		return "*" + typeName(value.Elem())
	case *types.Slice:
		return "[]" + typeName(value.Elem())
	case *types.Array:
		return fmt.Sprintf("[%d]%s", value.Len(), typeName(value.Elem()))
	case *types.Map:
		return "map[" + typeName(value.Key()) + "]" + typeName(value.Elem())
	}
	return types.TypeString(t, func(p *types.Package) string { return p.Path() })
}

func signature(s *types.Signature) node {
	params := []node{}
	for i := range s.Params().Len() {
		p := s.Params().At(i)
		params = append(params, node{"name": fmt.Sprint(i), "type": typeName(p.Type()), "required": !s.Variadic() || i != s.Params().Len()-1})
	}
	results := []string{}
	for i := range s.Results().Len() {
		results = append(results, typeName(s.Results().At(i).Type()))
	}
	generics := []node{}
	for i := range s.TypeParams().Len() {
		p := s.TypeParams().At(i)
		generics = append(generics, node{"name": p.Obj().Name(), "constraint": typeName(p.Constraint())})
	}
	return node{"params": params, "returns": results, "variadic": s.Variadic(), "type_parameters": generics}
}

func describe(obj types.Object) node {
	switch o := obj.(type) {
	case *types.Func:
		return node{"kind": "function", "signatures": []node{signature(o.Type().(*types.Signature))}}
	case *types.Const:
		result := node{"kind": "constant", "type": typeName(o.Type())}
		if o.Name() != "Version" && o.Name() != "ReleaseVersion" {
			result["value"] = o.Val().ExactString()
		}
		return result
	case *types.Var:
		return node{"kind": "variable", "type": typeName(o.Type())}
	case *types.TypeName:
		t := o.Type()
		result := node{"kind": "type", "alias": o.IsAlias()}
		if o.IsAlias() {
			result["type"] = typeName(types.Unalias(t))
			return result
		}
		named, ok := t.(*types.Named)
		if !ok {
			panic("unexpected exported type")
		}
		generics := []node{}
		for i := range named.TypeParams().Len() {
			p := named.TypeParams().At(i)
			generics = append(generics, node{"name": p.Obj().Name(), "constraint": typeName(p.Constraint())})
		}
		result["type_parameters"] = generics
		members := map[string]node{}
		switch under := named.Underlying().(type) {
		case *types.Struct:
			result["representation"] = "struct"
			for i := range under.NumFields() {
				f := under.Field(i)
				if !f.Exported() {
					continue
				}
				tag := reflect.StructTag(under.Tag(i)).Get("json")
				members[f.Name()] = node{"kind": "field", "type": typeName(f.Type()), "embedded": f.Embedded(), "json": tag, "required": tag != "" && tag != "-" && !strings.Contains(tag, ",omitzero") && !strings.Contains(tag, ",omitempty")}
			}
		case *types.Interface:
			result["representation"] = "interface"
			under.Complete()
			for i := range under.NumMethods() {
				f := under.Method(i)
				if f.Exported() {
					members[f.Name()] = node{"kind": "method", "required": true, "signatures": []node{signature(f.Type().(*types.Signature))}}
				}
			}
			terms := []string{}
			for i := range under.NumEmbeddeds() {
				terms = append(terms, typeName(under.EmbeddedType(i)))
			}
			result["embedded"] = terms
		default:
			result["representation"] = typeName(under)
		}
		for _, pointer := range []bool{false, true} {
			var receiver types.Type = named
			prefix := "value:"
			if pointer {
				receiver = types.NewPointer(named)
				prefix = "pointer:"
			}
			methods := types.NewMethodSet(receiver)
			for i := range methods.Len() {
				selection := methods.At(i)
				f := selection.Obj()
				if f.Exported() {
					members[prefix+f.Name()] = node{"kind": "method", "signatures": []node{signature(selection.Type().(*types.Signature))}}
				}
			}
		}
		result["members"] = members
		return result
	}
	panic(fmt.Sprintf("unsupported export %T", obj))
}

func main() {
	command := exec.Command("go", "list", "-export", "-deps", "-json", ".")
	command.Dir = os.Args[1]
	command.Stderr = os.Stderr
	bytes, err := command.Output()
	if err != nil {
		panic(err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(bytes)))
	exports := map[string]string{}
	target := ""
	for {
		var p struct {
			ImportPath, Export string
			DepOnly            bool
			Error              *struct{ Err string }
		}
		err := decoder.Decode(&p)
		if err == io.EOF {
			break
		}
		if err != nil {
			panic(err)
		}
		if p.Error != nil {
			panic(p.Error.Err)
		}
		exports[p.ImportPath] = p.Export
		if !p.DepOnly {
			target = p.ImportPath
		}
	}
	reader := importer.ForCompiler(token.NewFileSet(), "gc", func(path string) (io.ReadCloser, error) { return os.Open(exports[path]) })
	pkg, err := reader.Import(target)
	if err != nil {
		panic(err)
	}
	symbols := map[string]node{}
	for _, name := range pkg.Scope().Names() {
		obj := pkg.Scope().Lookup(name)
		if obj.Exported() {
			symbols[name] = describe(obj)
		}
	}
	if len(symbols) == 0 {
		panic("empty Go public surface")
	}
	if err := json.NewEncoder(os.Stdout).Encode(node{"symbols": symbols}); err != nil {
		panic(err)
	}
}
