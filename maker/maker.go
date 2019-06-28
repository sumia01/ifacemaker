package maker

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pkg/errors"

	"golang.org/x/tools/imports"
)

// Maker generates interfaces from structs.
type Maker struct {
	// StructName is the name of the struct from which to generate an interface.
	StructName string
	// If CopyDocs is true, doc comments will be copied to the generated interface.
	CopyDocs bool

	fset *token.FileSet

	importsByPath  map[string]*importedPkg
	importsByAlias map[string]*importedPkg
	imports        []*importedPkg
	methods        []*method
	methodNames    map[string]struct{}

	funcDecks                 []*ast.FuncDecl
	Output                    string
	PkgNameUsedInSourceStruct string
}

// errorAlias formats the alias for error messages.
// It replaces an empty string with "<none>".
func errorAlias(alias string) string {
	if alias == "" {
		return "<none>"
	}
	return alias
}

// ParseSource parses the source code in src.
// filename is used for position information only.
func (m *Maker) ParseSource(src []byte, filename string) error {
	currentPath, err := os.Getwd()
	if err != nil {
		return errors.Wrap(err, "os.Getwd()")
	}

	targetPath := filepath.Dir(currentPath + string(os.PathSeparator) + m.Output)

	if m.fset == nil {
		m.fset = token.NewFileSet()
	}
	if m.importsByPath == nil {
		m.importsByPath = make(map[string]*importedPkg)
	}
	if m.importsByAlias == nil {
		m.importsByAlias = make(map[string]*importedPkg)
	}
	if m.methods == nil {
		m.methodNames = make(map[string]struct{})
	}

	a, err := parser.ParseFile(m.fset, filename, src, parser.ParseComments)
	if err != nil {
		return errors.Wrap(err, "parsing file failed")
	}

	hasMethods := false
	for _, d := range a.Decls {
		if a, fd := m.getReceiverTypeName(d); a == m.StructName {
			if !fd.Name.IsExported() {
				continue
			}
			hasMethods = true
			methodName := fd.Name.String()
			if _, ok := m.methodNames[methodName]; ok {
				continue
			}

			m.funcDecks = append(m.funcDecks, fd)
		}
	}
	// No point checking imports if there are no relevant methods in this file.
	// This also avoids throwing unnecessary errors about imports in files that
	// are not relevant.
	if !hasMethods {
		return nil
	}

	for _, imp := range a.Imports {
		alias := ""
		if imp.Name != nil {
			alias = imp.Name.String()
		}
		if alias == "." {
			// Ignore dot imports.
			// Without parsing all the imported packages, we can't figure out
			// which ones are used by the interface, and which ones are not.
			// Goimports can't do this either.
			// However, we can't throw an error just because we find a
			// dot import when we're parsing all the files in a directory.
			// Let's assume that the struct we're building an
			// interface from doesn't use types from the dot import,
			// and everything will be fine.
			continue
		}
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			return errors.Wrapf(err, "parsing import `%v` failed", imp.Path.Value)
		}

		if strings.HasSuffix(targetPath, path) {
			if alias != "" {
				m.PkgNameUsedInSourceStruct = alias
			}
			continue
		}

		if existing, ok := m.importsByPath[path]; ok && existing.Alias != alias {
			// It would be possible to pick one alias and rewrite all the types,
			// but that would require parsing all the imports to find the correct
			// package name (which might differ from the import path's last element),
			// and that would require correctly finding the package in GOPATH
			// or vendor directories.
			return fmt.Errorf("Package %q imported multiple times with different aliases: %v, %v", path, errorAlias(existing.Alias), errorAlias(alias))
		} else if !ok {
			if alias != "" {
				if _, ok := m.importsByAlias[alias]; ok {
					return fmt.Errorf("Import alias %v already in use", alias)
				}
			}
			imp := &importedPkg{
				Path:  path,
				Alias: alias,
			}
			m.importsByPath[path] = imp
			m.importsByAlias[alias] = imp
			m.imports = append(m.imports, imp)
		}
	}

	for _, fd := range m.funcDecks {
		methodName := fd.Name.String()

		if _, ok := m.methodNames[methodName]; ok {
			continue
		}

		params, err := m.printParameters(fd.Type.Params)
		if err != nil {
			return errors.Wrap(err, "failed printing parameters")
		}

		ret, err := m.printParameters(fd.Type.Results)
		if err != nil {
			return errors.Wrap(err, "failed printing return values")
		}
		code := fmt.Sprintf("%s(%s) (%s)", methodName, params, ret)
		var docs []string
		if fd.Doc != nil && m.CopyDocs {
			for _, d := range fd.Doc.List {
				docs = append(docs, d.Text)
			}
		}
		m.methodNames[methodName] = struct{}{}
		m.methods = append(m.methods, &method{
			Code: code,
			Docs: docs,
		})
	}

	return nil
}

func (m *Maker) cleanParams(param string) string {
	expl := strings.Split(param, ".")
	if len(expl) == 2 && expl[0] == m.PkgNameUsedInSourceStruct {
		return expl[1]
	}

	return param
}

func (m *Maker) makeInterface(pkgName, ifaceName string) string {
	output := []string{
		"// Code generated by ifacemaker. DO NOT EDIT.",
		"",
		"package " + pkgName,
		"import (",
	}

	for _, pkgImport := range m.imports {
		if pkgImport != nil {
			output = append(output, pkgImport.Lines()...)
		}
	}
	output = append(output,
		")",
		fmt.Sprintf("type %s interface {", ifaceName),
	)

	for _, method := range m.methods {
		output = append(output, method.Lines()...)
	}
	output = append(output, "}")

	return strings.Join(output, "\n")
}

// MakeInterface creates the go file with the generated interface.
// The package will be named pkgName, and the interface will be named ifaceName.
func (m *Maker) MakeInterface(pkgName, ifaceName string) ([]byte, error) {
	unformatted := m.makeInterface(pkgName, ifaceName)
	b, err := formatCode(unformatted)
	if err != nil {
		err = errors.Wrapf(err, "Failed to format generated code. This could be a bug in ifacemaker. The generated code was:\n%v\nError", unformatted)
	}
	return b, err
}

// import resolution: sort imports by number of aliases.
// sort aliases by length ("" is unaliased).
// try all aliases. if all are already used up, generate a free one: pkgname + n,
// where n is a number so that the alias is free.

type method struct {
	Code string
	Docs []string
}

type importedPkg struct {
	Alias string
	Path  string
}

func (m *method) Lines() []string {
	var lines []string
	lines = append(lines, m.Docs...)
	lines = append(lines, m.Code)
	return lines
}

func (i *importedPkg) Lines() []string {
	var lines []string
	lines = append(lines, fmt.Sprintf("%v %q", i.Alias, i.Path))
	return lines
}

func (m *Maker) getReceiverTypeName(fl interface{}) (string, *ast.FuncDecl) {
	fd, ok := fl.(*ast.FuncDecl)
	if !ok {
		return "", nil
	}
	if fd.Recv.NumFields() != 1 {
		return "", nil
	}
	t := fd.Recv.List[0].Type
	if st, stok := t.(*ast.StarExpr); stok {
		t = st.X
	}

	ident, ok := t.(*ast.Ident)
	if !ok {
		return "", nil
	}
	return ident.Name, fd

}

func (m *Maker) printParameters(fl *ast.FieldList) (string, error) {
	if fl == nil {
		return "", nil
	}
	buff := &bytes.Buffer{}
	ll := len(fl.List)
	for ii, field := range fl.List {
		l := len(field.Names)
		for i, name := range field.Names {
			err := printer.Fprint(buff, m.fset, name)
			if err != nil {
				return "", errors.Wrap(err, "failed printing parameter name")
			}
			if i < l-1 {
				fmt.Fprint(buff, ",")
			} else {
				fmt.Fprint(buff, " ")
			}
		}
		currTypeBuf := &bytes.Buffer{}
		err := printer.Fprint(currTypeBuf, m.fset, field.Type)
		if err != nil {
			return "", errors.Wrap(err, "failed printing parameter type")
		}

		_, err = buff.WriteString(m.cleanParams(currTypeBuf.String()))
		if err != nil {
			return "", errors.Wrap(err, "buff.ReadFrom(currTypeBuf)")
		}
		if ii < ll-1 {
			fmt.Fprint(buff, ",")
		}
	}
	return buff.String(), nil
}

func formatCode(code string) ([]byte, error) {
	opts := &imports.Options{
		TabIndent: true,
		TabWidth:  2,
		Fragment:  true,
		Comments:  true,
	}
	return imports.Process("", []byte(code), opts)
}
