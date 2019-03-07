// Package wsdlgo provides an encoder from WSDL to Go code.
package wsdlgo

// TODO: make it generate code fully compliant with the spec.
// TODO: support all WSDL types.
// TODO: fully support SOAP bindings, faults, and transports.

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"github.com/fiorix/wsdl2go/wsdl"
	"golang.org/x/net/html/charset"
)

// An Encoder generates Go code from WSDL definitions.
type Encoder interface {
	// Encode generates Go code from d.
	Encode(d *wsdl.Definitions) error

	// SetPackageName sets some fmt.Stringer that can produce package name
	SetPackageName(packageName fmt.Stringer)

	// SetClient records the given http client that
	// is used when fetching remote parts of WSDL
	// and WSDL schemas.
	SetClient(c *http.Client)

	// SetAuthInfo allows setting of basic auth per domain
	SetAuthInfo(host string, user *url.Userinfo)

	// SetLocalNamespace allows overriding of the Namespace in XMLName instead
	// of the one specified in wsdl
	SetLocalNamespace(namespace string)

	// SetRequestVersion sets request version to set on every request
	SetRequestVersion(version string)

	// NoSimpleTypeIndirect Don't add * indirection to simple types
	NoSimpleTypeIndirect()

	// SetGoClientType allows overriding of the go client type in generated code
	// wsdl PortType name is used by default
	SetGoClientType(goClientTypeName string)

	// GenerateOnlyInterface generate only interface. Don't generate types
	GenerateOnlyInterface()

	// GenerateOnlyTypes generate only types. Don't generate interface.
	// Has priority over GenerateOnlyInterface
	GenerateOnlyTypes()
}

type goEncoder struct {
	// where to write Go code
	w io.Writer

	// http client
	http *http.Client
	// http basic credentials for host
	authInfo map[string]*url.Userinfo

	// some mechanism to name package
	packageName fmt.Stringer
	// go client type name
	goClientType string
	// requestVersion to set in request
	requestVersion string

	generateOnlyInterface bool
	generateOnlyTypes     bool
	noSimpleTypeIndirect  bool

	// types cache
	stypes        map[string]*wsdl.SimpleType
	ctypes        map[string]*wsdl.ComplexType
	typeAliases   map[string]string
	wroteOpStruct map[string]bool

	// elements cache
	elements map[string]*wsdl.Element

	// funcs cache
	funcs     map[string]*wsdl.Operation
	funcnames []string

	// messages cache
	messages map[string]*wsdl.Message

	// soap operations cache
	soapOps map[string]*wsdl.BindingOperation

	// whether to add supporting types
	needsDateType     bool
	needsTimeType     bool
	needsDateTimeType bool
	needsDurationType bool
	needsTag          map[string]string
	needsStdPkg       map[string]bool
	needsExtPkg       map[string]bool
	importedSchemas   map[string]bool
	usedNamespaces    map[string]string

	// localNamespace allows overriding of namespace in XMLName
	localNamespace string
}

// NewEncoder creates and initializes an Encoder that generates code to w.
func NewEncoder(w io.Writer) Encoder {
	return &goEncoder{
		w:               w,
		http:            http.DefaultClient,
		authInfo:        make(map[string]*url.Userinfo),
		stypes:          make(map[string]*wsdl.SimpleType),
		ctypes:          make(map[string]*wsdl.ComplexType),
		typeAliases:     make(map[string]string),
		wroteOpStruct:   make(map[string]bool),
		elements:        make(map[string]*wsdl.Element),
		funcs:           make(map[string]*wsdl.Operation),
		messages:        make(map[string]*wsdl.Message),
		soapOps:         make(map[string]*wsdl.BindingOperation),
		needsTag:        make(map[string]string),
		needsStdPkg:     make(map[string]bool),
		needsExtPkg:     make(map[string]bool),
		importedSchemas: make(map[string]bool),
	}
}

func (ge *goEncoder) SetPackageName(name fmt.Stringer) {
	ge.packageName = name
}

func (ge *goEncoder) SetClient(c *http.Client) {
	ge.http = c
}

// SetAuthInfo allows setting of basic auth per domain
func (ge *goEncoder) SetAuthInfo(host string, user *url.Userinfo) {
	if user == nil {
		return
	}

	ge.authInfo[host] = user
}

// SetRequestVersion sets request version to set on every request
func (ge *goEncoder) SetRequestVersion(version string) {
	ge.requestVersion = version
}

// SetGoClientType allows overriding of the go client type in generated code
// wsdl PortType name is used by default
func (ge *goEncoder) SetGoClientType(name string) {
	ge.goClientType = name
}

func (ge *goEncoder) getGoClientType(d *wsdl.Definitions) string {
	if ge.goClientType != "" {
		return ge.goClientType
	}

	return d.PortType.Name
}

// NoSimpleTypeIndirect Don't add * indirection to simple types
func (ge *goEncoder) NoSimpleTypeIndirect() {
	ge.noSimpleTypeIndirect = true
}

// GenerateOnlyInterface generate only interface. Don't generate types
func (ge *goEncoder) GenerateOnlyInterface() {
	ge.generateOnlyInterface = true
}

// GenerateOnlyTypes generate only types. Don't generate interface
// Has priority over GenerateOnlyInterface
func (ge *goEncoder) GenerateOnlyTypes() {
	ge.generateOnlyTypes = true
}

func gofmtPath() (string, error) {
	goroot := os.Getenv("GOROOT")
	if goroot != "" {
		return filepath.Join(goroot, "bin", "gofmt"), nil
	}
	return exec.LookPath("gofmt")

}

var numberSequence = regexp.MustCompile(`([a-zA-Z])(\d+)([a-zA-Z]?)`)
var numberReplacement = []byte(`$1 $2 $3`)

func addWordBoundariesToNumbers(s string) string {
	b := []byte(s)
	b = numberSequence.ReplaceAll(b, numberReplacement)
	return string(b)
}

func (ge *goEncoder) Encode(d *wsdl.Definitions) error {
	if d == nil {
		return nil
	}

	// default mechanism to set package name
	if ge.packageName == nil {
		ge.packageName = BindingPackageName(d.Binding)
	}

	var b bytes.Buffer
	err := ge.encode(&b, d)
	if err != nil {
		return err
	}
	if b.Len() == 0 {
		return nil
	}
	var errb bytes.Buffer
	input := b.String()

	// try to parse the generated code
	fset := token.NewFileSet()
	_, err = parser.ParseFile(fset, "", &b, parser.ParseComments)
	if err != nil {
		var src bytes.Buffer
		s := bufio.NewScanner(strings.NewReader(input))
		for line := 1; s.Scan(); line++ {
			fmt.Fprintf(&src, "%5d\t%s\n", line, s.Bytes())
		}
		return fmt.Errorf("generated bad code: %v\n%s", err, src.String())
	}

	// dat pipe to gofmt
	path, err := gofmtPath()
	if err != nil {
		return fmt.Errorf("cannot find gofmt with err: %v", err)
	}
	cmd := exec.Cmd{
		Path:   path,
		Stdin:  &b,
		Stdout: ge.w,
		Stderr: &errb,
	}
	err = cmd.Run()
	if err != nil {
		var x bytes.Buffer
		fmt.Fprintf(&x, "gofmt: %v\n", err)
		if errb.Len() > 0 {
			fmt.Fprintf(&x, "gofmt stderr:\n%s\n", errb.String())
		}
		fmt.Fprintf(&x, "generated code:\n%s\n", input)
		return fmt.Errorf(x.String())
	}
	return nil
}

func (ge *goEncoder) encode(w io.Writer, d *wsdl.Definitions) error {
	ge.unionSchemasData(d, &d.Schema)
	err := ge.importParts(d)
	ge.usedNamespaces = d.Namespaces
	if err != nil {
		return fmt.Errorf("wsdl import: %v", err)
	}
	ge.setNamespace(d)
	ge.cacheTypes(d)
	ge.cacheFuncs(d)
	ge.cacheMessages(d)
	ge.cacheSOAPOperations(d)
	ge.addVersionAttrToOpTypes()

	var b bytes.Buffer
	var ff []func(io.Writer, *wsdl.Definitions) error
	if len(ge.soapOps) > 0 {
		if ge.generateOnlyTypes {
			ff = append(ff,
				ge.writeGoTypes,
			)
		} else if ge.generateOnlyInterface {
			ff = append(ff,
				ge.writeInterfaceFuncs,
				ge.writeGoClientType,
				ge.writeGoClientFuncs,
			)
		} else {
			ff = append(ff,
				ge.writeInterfaceFuncs,
				ge.writeGoTypes,
				ge.writeGoClientType,
				ge.writeGoClientFuncs,
			)
		}
	} else {
		// TODO: probably faulty wsdl?
		if ge.generateOnlyTypes {
			ff = append(ff,
				ge.writeGoTypes,
			)
		} else if ge.generateOnlyInterface {
			ff = append(ff,
				ge.writeGoClientFuncs,
			)
		} else {
			ff = append(ff,
				ge.writeGoClientFuncs,
				ge.writeGoTypes,
			)
		}
	}
	for _, f := range ff {
		err := f(&b, d)
		if err != nil {
			return err
		}
	}

	fmt.Fprintf(w, "package %s\n\nimport (\n", ge.packageName)
	for pkg := range ge.needsStdPkg {
		fmt.Fprintf(w, "%q\n", pkg)
	}
	if len(ge.needsStdPkg) > 0 {
		fmt.Fprintf(w, "\n")
	}
	for pkg := range ge.needsExtPkg {
		fmt.Fprintf(w, "%q\n", pkg)
	}
	fmt.Fprintf(w, ")\n\n")
	if d.Schema.TargetNamespace != "" && !ge.generateOnlyInterface {
		ge.writeComments(w, "Namespace", "")
		fmt.Fprintf(w, "var Namespace = %q\n\n", d.Schema.TargetNamespace)
	}

	if ge.requestVersion != "" && !ge.generateOnlyInterface {
		ge.writeComments(w, "RequestVersion", "")
		fmt.Fprintf(w, "var RequestVersion = %q\n\n", ge.requestVersion)
	}

	_, err = io.Copy(w, &b)
	return err
}

func (ge *goEncoder) importParts(d *wsdl.Definitions) error {
	err := ge.importRoot(d)
	if err != nil {
		return err
	}
	return ge.importSchema(d)
}

func (ge *goEncoder) importRoot(d *wsdl.Definitions) error {
	for _, imp := range d.Imports {
		if imp.Location == "" {
			continue
		}
		err := ge.importRemote(imp.Location, &d)
		if err != nil {
			return err
		}
	}

	return nil
}

func (ge *goEncoder) importSchema(d *wsdl.Definitions) error {
	for _, imp := range d.Schema.Imports {
		err := ge.includeImport(d, imp.Location)
		if err != nil {
			return err
		}
	}

	for _, imp := range d.Schema.Includes {
		err := ge.includeImport(d, imp.Location)
		if err != nil {
			return err
		}
	}

	return nil
}

func (ge *goEncoder) includeImport(d *wsdl.Definitions, location string) error {
	if location == "" || ge.importedSchemas[location] {
		return nil
	}
	schema := &wsdl.Schema{}
	err := ge.importRemote(location, schema)
	if err != nil {
		return err
	}
	ge.unionSchemasData(d, schema)

	for _, item := range schema.Imports {
		ge.includeImport(d, item.Location)
	}
	for _, item := range schema.Includes {
		ge.includeImport(d, item.Location)
	}

	return nil
}

func (ge *goEncoder) unionSchemasData(d *wsdl.Definitions, s *wsdl.Schema) {
	for ns := range s.Namespaces {
		d.Namespaces[ns] = s.Namespaces[ns]
	}
	for _, ct := range s.ComplexTypes {
		ct.TargetNamespace = s.TargetNamespace
	}
	for _, st := range s.SimpleTypes {
		st.TargetNamespace = s.TargetNamespace
	}
	d.Schema.ComplexTypes = append(d.Schema.ComplexTypes, s.ComplexTypes...)
	d.Schema.SimpleTypes = append(d.Schema.SimpleTypes, s.SimpleTypes...)
	d.Schema.Elements = append(d.Schema.Elements, s.Elements...)
}

// download xml from url, decode in v.
func (ge *goEncoder) importRemote(location string, v interface{}) error {
	if ge.importedSchemas[location] {
		return nil
	}

	u, err := url.Parse(location)
	if err != nil {
		return err
	}

	var r io.Reader
	switch u.Scheme {
	case "http", "https":
		if u.User == nil {
			u.User = ge.authInfo[u.Host]
		}

		resp, err := ge.http.Get(u.String())
		if err != nil {
			return err
		}
		ge.importedSchemas[location] = true
		defer resp.Body.Close()
		r = resp.Body
	default:
		file, err := os.Open(u.Path)
		if err != nil {
			return fmt.Errorf("could not open file raw: %s path: %s escaped: %s : %v", u.RawPath, u.Path, u.EscapedPath(), err)
		}

		r = bufio.NewReader(file)
	}
	decoder := xml.NewDecoder(r)
	decoder.CharsetReader = charset.NewReaderLabel
	return decoder.Decode(&v)

}

func (ge *goEncoder) setNamespace(d *wsdl.Definitions) {
	for _, v := range d.Schema.Elements {
		if v.ComplexType != nil && v.ComplexType.TargetNamespace == "" {
			v.ComplexType.TargetNamespace = d.Schema.TargetNamespace
		}
	}
}

func (ge *goEncoder) cacheTypes(d *wsdl.Definitions) {
	// operation types are declared as go struct types
	ge.cacheTypesForElements(d.Schema.Elements, "")

	// simple types map 1:1 to go basic types
	for _, v := range d.Schema.SimpleTypes {
		ge.stypes[v.Name] = v
	}
	// complex types are declared as go struct types
	ge.cacheTypesForComplexTypes(d.Schema.ComplexTypes, "")

	// cache elements from schema
	ge.cacheElements(d.Schema.Elements)
	// cache elements from complex types
	for _, ct := range ge.ctypes {
		ge.cacheComplexTypeElements(ct)
	}

	for _, ct := range ge.ctypes {
		ge.flatTypeElements(ct)
	}
}

func (ge *goEncoder) cacheTypesForComplexTypes(complexTypes []*wsdl.ComplexType, typePrefix string) {
	for _, v := range complexTypes {
		ge.cacheTypesForComplexType(v, typePrefix)
	}
}

func (ge *goEncoder) cacheTypesForComplexType(v *wsdl.ComplexType, typeName string) {
	if v.Name != "" {
		typeName = v.Name
	}

	v.Name = typeName
	ge.ctypes[typeName] = v

	ge.cacheTypesForSequence(v.Sequence, typeName)

	choice := v.Choice
	if choice != nil {
		ge.cacheTypesForElements(choice.Elements, typeName)

		ge.cacheTypesForSequence(choice.Sequence, typeName)
	}

	cc := v.ComplexContent
	if cc != nil {
		cce := cc.Extension
		if cce != nil && cce.Sequence != nil {
			ge.cacheTypesForSequence(cce.Sequence, typeName)
		}
	}
}
func (ge *goEncoder) cacheTypesForSequence(seq *wsdl.Sequence, typeName string) {
	if seq == nil {
		return
	}
	ge.cacheTypesForComplexTypes(seq.ComplexTypes, "")
	ge.cacheTypesForElements(seq.Elements, typeName)

	for _, choice := range seq.Choices {
		if choice == nil {
			continue
		}

		ge.cacheTypesForComplexTypes(choice.ComplexTypes, "")
		ge.cacheTypesForElements(choice.Elements, typeName)
	}
}

func (ge *goEncoder) cacheTypesForElements(elements []*wsdl.Element, typePrefix string) {
	for _, v := range elements {
		typeName := typePrefix + v.Name
		if v.Name != "" && v.Type != "" {
			ge.typeAliases[typeName] = v.Type
		}
		if v.ComplexType == nil {
			continue
		}

		if v.Type == "" {
			ct := *v.ComplexType
			ct.Name = typeName
			ge.ctypes[typeName] = &ct
			v.Type = typeName
			// TODO Recognize type aliases through base: only extension base without fields and attrs
		}
		ge.cacheTypesForComplexType(v.ComplexType, typeName)
	}
}

func (ge *goEncoder) cacheChoiceTypeElements(choice *wsdl.Choice) {
	if choice != nil {
		for _, cct := range choice.ComplexTypes {
			ge.cacheComplexTypeElements(cct)
		}
		ge.cacheElements(choice.Elements)
	}
}

func (ge *goEncoder) cacheComplexTypeElements(ct *wsdl.ComplexType) {
	if ct.AllElements != nil {
		ge.cacheElements(ct.AllElements)
	}
	if ct.Sequence != nil {
		ge.cacheElements(ct.Sequence.Elements)
	}
	if ct.Choice != nil {
		ge.cacheElements(ct.Choice.Elements)
	}

	cc := ct.ComplexContent
	if cc != nil {
		cce := cc.Extension
		if cce != nil && cce.Sequence != nil {
			seq := cce.Sequence
			for _, cct := range seq.ComplexTypes {
				ge.cacheComplexTypeElements(cct)
			}
			ge.cacheElements(seq.Elements)

			//Add in Choice elements
			for _, choice := range seq.Choices {
				ge.cacheChoiceTypeElements(choice)
			}
		}
		if cce != nil && cce.Choice != nil {
			ge.cacheChoiceTypeElements(cce.Choice)
		}
	}
}

func (ge *goEncoder) cacheElements(ct []*wsdl.Element) {
	for _, el := range ct {
		if el.Name == "" || el.Type == "" {
			if el.Ref == "" {
				continue
			}
			el.Name = trimns(el.Ref)
			el.Type = el.Name
		}
		name := trimns(el.Name)

		if _, exists := ge.elements[name]; exists {
			continue
		}
		ge.elements[name] = el

		if ge.typeAliases[el.Type] != "" {
			el.Type = ge.typeAliases[el.Type]
		}
		ct := el.ComplexType
		if ct != nil {
			ge.cacheElements(ct.AllElements)
			if ct.Sequence != nil {
				ge.cacheElements(ct.Sequence.Elements)
			}
			if ct.Choice != nil {
				ge.cacheElements(ct.Choice.Elements)
			}
		}
	}
}

func (ge *goEncoder) flatTypeElements(ct *wsdl.ComplexType) {
	if ct.Sequence == nil {
		ct.Sequence = &wsdl.Sequence{}
	}

	ge.flatComplexContent(ct)
	ge.flatSimpleContent(ct)
	ge.flatFields(ct)
}

func (ge *goEncoder) flatComplexContent(ct *wsdl.ComplexType) {
	if ct.ComplexContent == nil || ct.ComplexContent.Extension == nil {
		return
	}

	ext := ct.ComplexContent.Extension

	ct.ComplexContent.Extension = nil
	if ct.ComplexContent.Restriction == nil {
		ct.ComplexContent = nil
	}

	if ext.Base != "" {
		if base, exists := ge.ctypes[trimns(ext.Base)]; exists {
			ct.Sequence.ComplexTypes = append(ct.Sequence.ComplexTypes, base)
		}
	}

	ct.Attributes = ge.flatAttributeFields(ct.Attributes, ext.Attributes, RedefinedStructFields{})

	sequences := make([]*wsdl.Sequence, 0)
	if ext.Sequence != nil {
		sequences = append(sequences, ext.Sequence)
		for _, choice := range ext.Sequence.Choices {
			seq := &wsdl.Sequence{
				ComplexTypes: choice.ComplexTypes,
				Elements:     choice.Elements,
				Any:          choice.Any}
			sequences = append(sequences, seq)
		}
	}
	if ext.Choice != nil {
		seq := &wsdl.Sequence{
			ComplexTypes: ext.Choice.ComplexTypes,
			Elements:     ext.Choice.Elements,
			Any:          ext.Choice.Any}
		sequences = append(sequences, seq)

		if ext.Choice.Sequence != nil {
			sequences = append(sequences, ext.Choice.Sequence)
		}
	}

	for _, seq := range sequences {
		if seq.ComplexTypes != nil {
			ct.Sequence.ComplexTypes = append(ct.Sequence.ComplexTypes, seq.ComplexTypes...)
		}
		if seq.Elements != nil {
			ct.Sequence.Elements = append(ct.Sequence.Elements, seq.Elements...)
		}
		if seq.Any != nil {
			ct.Sequence.Any = append(ct.Sequence.Any, seq.Any...)
		}
	}
}

func (ge *goEncoder) flatSimpleContent(ct *wsdl.ComplexType) {
	if ct.SimpleContent == nil || ct.SimpleContent.Extension == nil {
		return
	}

	ext := ct.SimpleContent.Extension

	ct.SimpleContent.Extension = nil
	if ct.SimpleContent.Restriction == nil {
		ct.SimpleContent = nil
	}

	ct.Attributes = ge.flatAttributeFields(ct.Attributes, ext.Attributes, RedefinedStructFields{})

	if ext.Base != "" {
		baseComplex, exists := ge.ctypes[trimns(ext.Base)]
		if exists {
			ct.Sequence.ComplexTypes = append(ct.Sequence.ComplexTypes, baseComplex)
		} else {
			// otherwise it's a simple type
			el := &wsdl.Element{
				Type: trimns(ext.Base),
				Name: "Content",
				Tag:  ",chardata",
			}
			ct.AllElements = ge.flatElementField(ct.AllElements, el, RedefinedStructFields{})
		}
	}

	// sequence, choice, etc. are not supported in simpleContent tags.
}

func (ge *goEncoder) flatFields(ct *wsdl.ComplexType) {
	redefined := RedefinedStructFields{}

	if ct.Sequence != nil {
		for _, v := range ct.Sequence.ComplexTypes {
			ge.flatTypeElements(v)

			ct.Attributes = ge.flatAttributeFields(ct.Attributes, v.Attributes, RedefinedStructFields{})  //redefined)
			ct.AllElements = ge.flatElementFields(ct.AllElements, v.AllElements, RedefinedStructFields{}) //redefined)
		}

		ct.AllElements = ge.flatElementFields(ct.AllElements, ct.Sequence.Elements, redefined)

		for _, choice := range ct.Sequence.Choices {
			ct.AllElements = ge.flatElementFields(ct.AllElements, choice.Elements, redefined)
		}
	}
	ct.Sequence = nil

	if ct.Choice != nil {
		ct.AllElements = ge.flatElementFields(ct.AllElements, ct.Choice.Elements, redefined)

		if ct.Choice.Sequence != nil {
			ct.AllElements = ge.flatElementFields(ct.AllElements, ct.Choice.Sequence.Elements, redefined)
		}
	}
	ct.Choice = nil
}

func (ge *goEncoder) flatAttributeFields(attrs []*wsdl.Attribute, appendedAttrs []*wsdl.Attribute, redefined RedefinedStructFields) []*wsdl.Attribute {
	for _, attr := range appendedAttrs {
		attrs = ge.flatAttributeField(attrs, attr, redefined)
	}

	return attrs
}

func (ge *goEncoder) flatAttributeField(attrs []*wsdl.Attribute, attr *wsdl.Attribute, redefined RedefinedStructFields) []*wsdl.Attribute {
	if redefined[attr.Name] {
		return attrs
	}
	redefined[attr.Name] = true

	return append(attrs, attr)
}

func (ge *goEncoder) flatElementFields(elements []*wsdl.Element, appendedElements []*wsdl.Element, redefined RedefinedStructFields) []*wsdl.Element {
	for _, attr := range appendedElements {
		elements = ge.flatElementField(elements, attr, redefined)
	}

	return elements
}

func (ge *goEncoder) flatElementField(elements []*wsdl.Element, el *wsdl.Element, redefined RedefinedStructFields) []*wsdl.Element {
	if redefined[el.Name] {
		return elements
	}
	redefined[el.Name] = true

	return append(elements, el)
}

func (ge *goEncoder) cacheFuncs(d *wsdl.Definitions) {
	// operations are declared as boilerplate go functions
	for _, v := range d.PortType.Operations {
		ge.funcs[v.Name] = v
	}
	ge.funcnames = make([]string, len(ge.funcs))
	i := 0
	for k := range ge.funcs {
		ge.funcnames[i] = k
		i++
	}
	sort.Strings(ge.funcnames)
}

func (ge *goEncoder) cacheMessages(d *wsdl.Definitions) {
	for _, v := range d.Messages {
		ge.messages[v.Name] = v
	}
}

func (ge *goEncoder) cacheSOAPOperations(d *wsdl.Definitions) {
	for _, v := range d.Binding.Operations {
		ge.soapOps[v.Name] = v
	}
}

func (ge *goEncoder) addVersionAttrToOpTypes() {
	if ge.requestVersion != "" {
		typeNamespace := &wsdl.Attribute{
			XMLName:  xml.Name{Space: "http://www.w3.org/2001/XMLSchema", Local: "attribute"},
			Name:     "TypeNamespace",
			TagName:  "xmlns",
			Type:     "xsd:string",
			Nillable: false,
		}
		version := &wsdl.Attribute{
			XMLName:  xml.Name{Space: "http://www.w3.org/2001/XMLSchema", Local: "attribute"},
			Name:     "Version",
			Type:     "xsd:string",
			Nillable: false,
		}

		for _, f := range ge.funcs {
			inputType := ge.messages[trimns(f.Input.Message)].Name

			if ct, ok := ge.ctypes[inputType]; ok {
				ct.Attributes = ge.flatAttributeField(ct.Attributes, typeNamespace, RedefinedStructFields{})
				ct.Attributes = ge.flatAttributeField(ct.Attributes, version, RedefinedStructFields{})
			}
		}
	}
}

var interfaceTypeT = template.Must(template.New("interfaceType").Parse(`
// new{{.Name}} creates an initializes a {{.Name}}.
func new{{.Name}}(cli *client) {{.Name}} {
	return &{{.Impl}}{cli}
}

// {{.Name}} was auto-generated from WSDL
// and defines interface for the remote service. Useful for testing.
type {{.Name}} interface {
{{- range .Funcs }}
{{.Doc}}{{.Name}}({{.Input}}) ({{.Output}})
{{ end }}
}
`))

type interfaceTypeFunc struct{ Doc, Name, Input, Output string }

// writeInterfaceFuncs writes Go interface definitions from WSDL types to w.
// Functions are written in the same order of the WSDL document.
func (ge *goEncoder) writeInterfaceFuncs(w io.Writer, d *wsdl.Definitions) error {
	funcs := make([]*interfaceTypeFunc, len(ge.funcs))
	// Looping over the operations to determine what are the interface
	// functions.
	i := 0
	for _, fn := range ge.funcnames {
		op := ge.funcs[fn]
		if _, exists := ge.soapOps[op.Name]; !exists {
			// TODO: probably faulty wsdl?
			continue
		}
		inParams, err := ge.inputParams(op)
		if err != nil {
			return err
		}
		outParams, err := ge.outputParams(op)
		if err != nil {
			return err
		}
		in, out := code(inParams), codeParams(outParams)
		name := goSymbol(op.Name)
		var doc bytes.Buffer
		ge.writeComments(&doc, name, op.Doc)
		funcs[i] = &interfaceTypeFunc{
			Doc:    doc.String(),
			Name:   name,
			Input:  ge.inputArgs(in),
			Output: strings.Join(out, ","),
		}
		i++
	}
	n := ge.getGoClientType(d)
	return interfaceTypeT.Execute(w, &struct {
		Name  string
		Impl  string // private type that implements the interface
		Funcs []*interfaceTypeFunc
	}{
		goSymbol(n),
		strings.ToLower(n)[:1] + n[1:],
		funcs[:i],
	})
}

var portTypeT = template.Must(template.New("portType").Parse(`
// {{.Name}} implements the {{.Interface}} interface.
type {{.Name}} struct {
	cli *client
}

`))

func (ge *goEncoder) inputArgs(in []string) string {
	return strings.Join(append([]string{"ctx context.Context"}, in...), ",")
}

func (ge *goEncoder) writeGoClientType(w io.Writer, d *wsdl.Definitions) error {
	if len(ge.funcs) == 0 {
		return nil
	}
	n := ge.getGoClientType(d)
	return portTypeT.Execute(w, &struct {
		Name      string
		Interface string
	}{
		strings.ToLower(n)[:1] + n[1:],
		goSymbol(n),
	})
}

// writeGoClientFuncs writes Go function definitions from WSDL types to w.
// Functions are written in the same order of the WSDL document.
func (ge *goEncoder) writeGoClientFuncs(w io.Writer, d *wsdl.Definitions) error {
	ge.needsStdPkg["context"] = true
	ge.needsStdPkg["encoding/xml"] = true

	if d.Binding.Type != "" {
		a, b := trimns(d.Binding.Type), trimns(d.PortType.Name)
		if a != b {
			return fmt.Errorf(
				"binding %q requires port type %q but it's not defined",
				d.Binding.Name, d.Binding.Type)
		}
	}
	if len(ge.funcs) == 0 {
		return nil
	}
	for _, fn := range ge.funcnames {
		op := ge.funcs[fn]
		ge.writeComments(w, op.Name, op.Doc)
		inParams, err := ge.inputParams(op)
		if err != nil {
			return err
		}
		outParams, err := ge.outputParams(op)
		if err != nil {
			return err
		}

		ok := ge.writeSOAPFunc(w, d, op, inParams, outParams)
		if !ok {
			in, out := code(inParams), codeParams(outParams)
			ret := make([]string, len(out))
			for i, p := range outParams {
				ret[i] = ge.wsdl2goDefault(p.dataType)
			}

			ge.needsStdPkg["errors"] = true
			ge.needsStdPkg["context"] = true

			fn := ge.fixFuncNameConflicts(goSymbol(op.Name))
			fmt.Fprintf(w, "func %s(%s) (%s) {\nreturn %s\n}\n\n",
				fn,
				ge.inputArgs(in),
				strings.Join(out, ","),
				strings.Join(ret, ","),
			)
		}
	}
	return nil
}

var soapFuncT = template.Must(template.New("soapFunc").Parse(
	`func (c *{{.GoClientType}}) {{.Name}}({{.Input}}) ({{.Output}}) {
	α := struct {
		{{if .OpInputDataType}}
			{{if .RPCStyle}}M{{end}} {{.OpInputDataType}} ` + "`xml:\"{{.OpName}}\"`" + `
		{{end}}
	}{
		{{if .OpInputDataType}}{{.OpInputDataType}} {
			{{range $index, $element := .InputNames}}{{$element}},
			{{end}}
		},{{end}}
	}

	γ := struct {
		{{if .OpResponseDataType}}
			{{if .RPCStyle}}M {{end}}{{.OpResponseDataType}} ` + "`xml:\"{{.OpResponseName}}\"`" + `
		{{end}}
	}{}
	if err := c.cli.RoundTripWithAction("{{.Name}}", α, &γ); err != nil {
		return {{.RetDef}}
	}
	return {{range $index, $element := .OpOutputNames}}{{index $.OpOutputPrefixes $index}}γ.{{if $.RPCStyle}}M.{{end}}{{$element}}, {{end}}nil
}
`))

var soapActionFuncT = template.Must(template.New("soapActionFunc").Parse(
	`func (c *{{.GoClientType}}) {{.Name}}({{.Input}}) ({{.Output}}) {
    {{index .InputNames 0}}.Version = RequestVersion
    {{index .InputNames 0}}.TypeNamespace = Namespace
    {{index .InputNames 0}}.Party = c.cli.party

	req := struct {
		{{if .OpInputDataType}}
			{{if .RPCStyle}}M{{end}} {{.OpInputDataType}} ` + "`xml:\"{{.OpName}}\"`" + `
		{{end}}
	}{
		{{if .OpInputDataType}}{{.OpInputDataType}} {
			{{range $index, $element := .InputNames}}{{$element}},
			{{end}}
		},{{end}}
	}

	res := struct {
		XMLName  xml.Name ` + "`xml:\"Envelope\"`" + `
		XMLNSNS2 string   ` + "`xml:\"xmlns:ns2,attr\"`" + `
		XMLNSNS3 string   ` + "`xml:\"xmlns:ns3,attr\"`" + `
        Body     struct {
            {{if .OpResponseDataType}}
                {{if .RPCStyle}}M {{end}}{{index .OpOutputNames 0}} {{index .OpOutputNames 0}}
            {{end}}
        } ` + "`xml:\"Body\"`" + `
	}{}

    err := c.cli.call(ctx, "{{.Name}}", req, &res)
    if err != nil {
		return nil, err
    }

    return &res.Body.{{index .OpOutputNames 0}}, nil
}
`))

func (ge *goEncoder) writeSOAPFunc(w io.Writer, d *wsdl.Definitions, op *wsdl.Operation, in, out []*parameter) bool {
	if _, exists := ge.soapOps[op.Name]; !exists {
		// TODO: probably faulty wsdl?
		return false
	}

	// Do we need to wrap into a operation element?
	rpcStyle := false

	if d.Binding.BindingType != nil {
		rpcStyle = d.Binding.BindingType.Style == "rpc"
	}

	// inputNames describe the accessors to the input parameter names
	inputNames := make([]string, len(in))
	for index, name := range in {
		returnVal := maskKeywordUsage(name.code)

		if !strings.HasPrefix(name.dataType, "*") {
			returnVal = "&" + returnVal
		}

		inputNames[index] = returnVal
	}

	// outputDataTypes describe the data types which are returned by the func
	outputDataTypes := make([]string, len(out))

	// retDefaults describes the default return values in case of an error
	retDefaults := make([]string, len(out))

	// operationOutputNames describes the fields which are part of the response we unmarshal
	// len-1, because the last parameter is error, which is not part of the xml response we unmarshal
	operationOutputNames := make([]string, len(out)-1)
	operationOutputPrefixes := make([]string, len(out)-1)

	for index, name := range out {
		outputDataTypes[index] = name.dataType

		// operationOutputNames names will only be computed till len-1
		if index == len(out)-1 {
			continue
		}

		operationOutputNames[index] = strings.ToUpper(name.code[:1]) + name.code[1:]
		operationOutputPrefixes[index] = ""
		retDefaults[index] = "nil"

		// If the output is >not< a pointer, we need to return the value of the response
		if !strings.HasPrefix(name.dataType, "*") {
			operationOutputPrefixes[index] = "*"

			// Also - only resolve the default for non-pointer returns (otherwise nil suffices)
			retDefaults[index] = ge.wsdl2goDefault(name.dataType)
		}
	}
	retDefaults[len(retDefaults)-1] = "err"

	// Check if we need to prefix the op with a namespace
	namespacedOpName := op.Name
	nsSplit := strings.Split(ge.funcs[op.Name].Input.Message, ":")
	if len(nsSplit) > 1 {
		namespacedOpName = nsSplit[0] + ":" + namespacedOpName
	}

	// The response name is always the operation name + "Response" according to specification.
	// Note, we also omit the namespace, since this does currently not work reliable with golang
	// (See: https://github.com/golang/go/issues/14407)
	opResponseName := op.Name + "Response"

	// No-input operations can be inlined into an anonymous struct on rpc, and omitted otherwise
	operationInputDataType := ""

	if len(in) > 0 && op.Input != nil {
		operationInputDataType = ge.sanitizedOperationsType(ge.messages[trimns(op.Input.Message)].Name)
	} else if rpcStyle {
		operationInputDataType = "struct{}"
	}

	// No-output operations can be inlined into an anonymous struct on rpc, and omitted otherwise
	operationOutputDataType := ""

	if len(out) > 0 && op.Output != nil {
		operationOutputDataType = ge.sanitizedOperationsType(ge.messages[trimns(op.Output.Message)].Name)
	} else if rpcStyle {
		operationInputDataType = "struct{}"
	}

	goClientType := ge.getGoClientType(d)
	goClientType = strings.ToLower(goClientType[:1]) + goClientType[1:]

	input := ge.inputArgs(code(in))

	soapFunctionName := "RoundTripSoap12"
	soapAction := ""

	if bindingOp, exists := ge.soapOps[op.Name]; exists {
		soapAction = bindingOp.Operation.Action
		if soapAction == "" {
			soapFunctionName = "RoundTripWithAction"
			soapAction = bindingOp.Operation11.Action
		}
	}
	if soapAction != "" {
		soapActionFuncT.Execute(w, &struct {
			RoundTripType      string
			Action             string
			GoClientType       string
			Name               string
			OpName             string
			OpInputDataType    string
			InputNames         []string
			OpResponseName     string
			OpResponseDataType string
			OpOutputNames      []string
			OpOutputPrefixes   []string
			Input              string
			Output             string
			RetDef             string
			RPCStyle           bool
		}{
			soapFunctionName,
			soapAction,
			goClientType,
			goSymbol(op.Name),
			namespacedOpName,
			operationInputDataType,
			inputNames,
			opResponseName,
			operationOutputDataType,
			operationOutputNames,
			operationOutputPrefixes,
			input,
			strings.Join(outputDataTypes, ","),
			strings.Join(retDefaults, ","),
			rpcStyle,
		})
		return true
	}
	soapFuncT.Execute(w, &struct {
		GoClientType       string
		Name               string
		OpName             string
		OpInputDataType    string
		InputNames         []string
		OpResponseName     string
		OpResponseDataType string
		OpOutputNames      []string
		OpOutputPrefixes   []string
		Input              string
		Output             string
		RetDef             string
		RPCStyle           bool
	}{
		goClientType,
		goSymbol(op.Name),
		namespacedOpName,
		operationInputDataType,
		inputNames,
		opResponseName,
		operationOutputDataType,
		operationOutputNames,
		operationOutputPrefixes,
		input,
		strings.Join(outputDataTypes, ","),
		strings.Join(retDefaults, ","),
		rpcStyle,
	})
	return true
}

func renameParam(p, name string) string {
	v := strings.SplitN(p, " ", 2)
	if len(v) != 2 {
		return p
	}
	return name + " " + v[1]
}

// returns list of function input parameters.
func (ge *goEncoder) inputParams(op *wsdl.Operation) ([]*parameter, error) {
	if op.Input == nil {
		return []*parameter{}, nil
	}
	im := trimns(op.Input.Message)
	req, ok := ge.messages[im]
	if !ok {
		return nil, fmt.Errorf("operation %q wants input message %q but it's not defined", op.Name, im)
	}

	// TODO: I had to disable this for my use case - do other use cases still work with false?
	return ge.genParams(req, false), nil
}

// returns list of function output parameters plus error.
func (ge *goEncoder) outputParams(op *wsdl.Operation) ([]*parameter, error) {
	out := []*parameter{{code: "err", dataType: "error"}}

	if op.Output == nil {
		return out, nil
	}
	om := trimns(op.Output.Message)
	resp, ok := ge.messages[om]
	if !ok {
		return nil, fmt.Errorf("operation %q wants output message %q but it's not defined", op.Name, om)
	}
	return append(ge.genParams(resp, false), out[0]), nil
}

var isGoKeyword = map[string]bool{
	"break":       true,
	"case":        true,
	"chan":        true,
	"const":       true,
	"continue":    true,
	"default":     true,
	"else":        true,
	"defer":       true,
	"fallthrough": true,
	"for":         true,
	"func":        true,
	"go":          true,
	"goto":        true,
	"if":          true,
	"import":      true,
	"interface":   true,
	"map":         true,
	"package":     true,
	"range":       true,
	"return":      true,
	"select":      true,
	"struct":      true,
	"switch":      true,
	"type":        true,
	"var":         true,
}

type parameter struct {
	code     string
	dataType string
	xmlToken string
}

func code(list []*parameter) []string {
	code := make([]string, len(list))
	for i, p := range list {
		code[i] = maskKeywordUsage(p.code) + " " + p.dataType
	}
	return code
}

func codeParams(list []*parameter) []string {
	code := make([]string, len(list))
	for i, p := range list {
		code[i] = p.dataType
	}
	return code
}

func maskKeywordUsage(code string) string {
	returnVal := code

	if isGoKeyword[code] {
		returnVal = "_" + code
	}

	return returnVal
}

func (ge *goEncoder) genParams(m *wsdl.Message, needsTag bool) []*parameter {
	params := make([]*parameter, len(m.Parts))
	for i, param := range m.Parts {
		code := param.Name
		var t, token, elName string
		switch {
		case param.Type != "":
			t = ge.wsdl2goType(param.Type)
			elName = trimns(param.Type)
			token = t
		case param.Element != "":
			elName = trimns(param.Element)
			code = goSymbol(param.Element)
			if el, ok := ge.elements[elName]; ok {
				t = ge.wsdl2goType(trimns(el.Type))
			} else {
				t = ge.wsdl2goType(param.Element)
			}
			token = trimns(param.Element)
		}
		params[i] = &parameter{code: code, dataType: t, xmlToken: token}
		if needsTag {
			ge.needsStdPkg["encoding/xml"] = true
			ge.needsTag[strings.TrimPrefix(t, "*")] = elName
		}
	}
	return params
}

// Fixes conflicts between function and type names.
func (ge *goEncoder) fixFuncNameConflicts(name string) string {
	if _, exists := ge.stypes[name]; exists {
		name += "Func"
		return ge.fixFuncNameConflicts(name)
	}
	if _, exists := ge.ctypes[name]; exists {
		name += "Func"
		return ge.fixFuncNameConflicts(name)
	}
	return name
}

// Fixes request and response parameters with the same name, in place.
// Each string in the slice consists of Go's "name Type", we only
// compare names. In case of a conflict, we set the response one
// in the form of respName.
func (ge *goEncoder) fixParamConflicts(req, resp []string) {
	for _, a := range req {
		for j, b := range resp {
			x := strings.SplitN(a, " ", 2)[0]
			y := strings.SplitN(b, " ", 2)
			if len(y) > 1 {
				if x == y[0] {
					n := goSymbol(y[0])
					resp[j] = "resp" + n + " " + y[1]
				}
			}
		}
	}
}

// Helps to clean up operation names, so we can generate
// nice datatype names which make golang happy.
// E.g. - a soap operation gkstServer_getVersion is sanitized
// to gkstServerGetVersion (remove snake case)
func (ge *goEncoder) sanitizedOperationsType(opName string) string {
	return "Operation" + goSymbol(opName)
}

// Converts types from wsdl type to Go type.
func (ge *goEncoder) wsdl2goType(t string) string {
	// TODO: support other types.
	v := trimns(t)
	if _, exists := ge.stypes[v]; exists {
		return goSymbol(v)
	}
	switch strings.ToLower(v) {
	case "byte", "unsignedbyte":
		return "byte"
	case "int":
		return "int"
	case "integer":
		return "int64" // todo: replace this with math/big since integer is infinite set
	case "long":
		return "int64"
	case "float", "double", "decimal":
		return "float64"
	case "boolean":
		return "bool"
	case "hexbinary", "base64binary":
		return "[]byte"
	case "string", "anyuri", "token", "nmtoken", "qname", "language", "id":
		return "string"
	case "date":
		ge.needsDateType = true
		return "Date"
	case "time":
		ge.needsTimeType = true
		return "Time"
	case "nonnegativeinteger":
		return "uint"
	case "positiveinteger":
		return "uint64"
	case "normalizedstring":
		return "string"
	case "unsignedint":
		return "uint"
	case "datetime":
		ge.needsDateTimeType = true
		return "DateTime"
	case "duration":
		ge.needsDurationType = true
		return "Duration"
	case "anysequence", "anytype", "anysimpletype":
		return "interface{}"
	case "idref":
		return "string"
	case "idrefs":
		return "string"
	default:
		v = goSymbol(v)
		if ge.ctypes[v] == nil {
			return "string"
		}
		return "*" + v
	}
}

// Returns the default Go type for the given wsdl type.
func (ge *goEncoder) wsdl2goDefault(t string) string {
	v := trimns(t)
	if v != "" && v[0] == '*' {
		v = v[1:]
	}
	switch v {
	case "error":
		return `errors.New("not implemented")`
	case "bool":
		return "false"
	case "uint", "int", "int64", "float64":
		return "0"
	case "string":
		return `""`
	case "[]byte", "interface{}":
		return "nil"
	default:
		return "&" + v + "{}"
	}
}

func (ge *goEncoder) renameType(old, name string) {
	// TODO: rename Elements that point to this type also?
	ct, exists := ge.ctypes[old]
	if !exists {
		old = trimns(old)
		ct, exists = ge.ctypes[old]
		if !exists {
			return
		}
		name = trimns(name)
	}
	ct.Name = name
	delete(ge.ctypes, old)
	ge.ctypes[name] = ct
}

// writeGoTypes writes Go types from WSDL types to w.
//
// Types are written in this order, alphabetically: date types that we
// generate, simple types, then complex types.
func (ge *goEncoder) writeGoTypes(w io.Writer, d *wsdl.Definitions) error {
	var b bytes.Buffer
	for _, name := range ge.sortedSimpleTypes() {
		st := ge.stypes[name]
		stname := goSymbol(st.Name)
		if st.Restriction != nil {
			ge.writeComments(&b, stname, "")
			fmt.Fprintf(&b, "type %s %s\n\n", stname, ge.wsdl2goType(st.Restriction.Base))
			ge.genValidator(&b, stname, st.Restriction)
		} else if st.Union != nil {
			types := strings.Split(st.Union.MemberTypes, " ")
			ntypes := make([]string, 0, len(types))
			baseTypes := make(map[string]struct{}, len(types))
			for _, t := range types {
				t = strings.TrimSpace(t)
				if t == "" {
					continue
				}
				ntypes = append(ntypes, ge.wsdl2goType(t))
				if st, ok := ge.stypes[t]; ok {
					if st.Restriction == nil {
						continue
					}
					baseTypes[st.Restriction.Base] = struct{}{}
				} else {
					panic(fmt.Sprintf("Union for '%s': simple type '%s' must be in ge.stypes", stname, t))
				}
			}
			for _, t := range st.Union.SimpleTypes {
				ntypes = append(ntypes, "ANONIMOUS")
				if t.Restriction == nil {
					continue
				}
				baseTypes[t.Restriction.Base] = struct{}{}
			}

			doc := stname + " is a union of: " + strings.Join(ntypes, ", ")
			ge.writeComments(&b, stname, doc)
			if len(baseTypes) == 1 {
				for baseType := range baseTypes {
					fmt.Fprintf(&b, "type %s %s\n\n", stname, ge.wsdl2goType(baseType))
				}
			} else {
				fmt.Fprintf(&b, "type %s interface{}\n\n", stname)
			}
		}
	}
	var err error
	for _, name := range ge.sortedComplexTypes() {
		ct := ge.ctypes[name]
		err = ge.genGoStruct(&b, d, ct)
		if err != nil {
			return err
		}
		ge.genGoXMLTypeFunction(&b, ct)
	}

	// Operation wrappers - mainly used for rpc, not exclusively
	for _, name := range ge.sortedOperations() {
		ct := ge.soapOps[name]

		err = ge.genGoOpStruct(&b, d, ct)
		if err != nil {
			return err
		}
	}

	ge.genDateTypes(w) // must be called last
	_, err = io.Copy(w, &b)
	return err
}

func (ge *goEncoder) sortedSimpleTypes() []string {
	keys := make([]string, len(ge.stypes))
	i := 0
	for k := range ge.stypes {
		keys[i] = k
		i++
	}
	sort.Strings(keys)
	return keys
}

func (ge *goEncoder) sortedComplexTypes() []string {
	keys := make([]string, len(ge.ctypes))
	i := 0
	for k := range ge.ctypes {
		keys[i] = k
		i++
	}
	sort.Strings(keys)
	return keys
}

func (ge *goEncoder) sortedOperations() []string {
	keys := make([]string, len(ge.soapOps))
	i := 0
	for k := range ge.soapOps {
		keys[i] = k
		i++
	}
	sort.Strings(keys)
	return keys
}

func (ge *goEncoder) genDateTypes(w io.Writer) {
	cases := []struct {
		needs bool
		name  string
		code  string
	}{
		{
			needs: ge.needsDateType,
			name:  "Date",
			code:  "type Date string\n\n",
		},
		{
			needs: ge.needsTimeType,
			name:  "Time",
			code:  "type Time string\n\n",
		},
		{
			needs: ge.needsDateTimeType,
			name:  "DateTime",
			code:  "type DateTime string\n\n",
		},
		{
			needs: ge.needsDurationType,
			name:  "Duration",
			code:  "type Duration string\n\n",
		},
	}
	for _, c := range cases {
		if !c.needs {
			continue
		}
		ge.writeComments(w, c.name, c.name+" in WSDL format.")
		io.WriteString(w, c.code)
	}
}

var validatorT = template.Must(template.New("validator").Parse(`
// Validate validates {{.TypeName}}.
func (v {{.TypeName}}) Validate() bool {
	for _, vv := range []{{.Type}} {
		{{range .Args}}{{.}},{{"\n"}}{{end}}
	}{
		if reflect.DeepEqual(v, vv) {
			return true
		}
	}
	return false
}
`))

func (ge *goEncoder) genValidator(w io.Writer, typeName string, r *wsdl.Restriction) {
	if len(r.Enum) == 0 {
		return
	}
	args := make([]string, len(r.Enum))
	t := ge.wsdl2goType(r.Base)
	for i, v := range r.Enum {
		if t == "string" {
			args[i] = strconv.Quote(v.Value)
		} else {
			args[i] = v.Value
		}
	}
	ge.needsStdPkg["reflect"] = true
	validatorT.Execute(w, &struct {
		TypeName string
		Type     string
		Args     []string
	}{
		typeName,
		t,
		args,
	})
}

func (ge *goEncoder) genGoXMLTypeFunction(w io.Writer, ct *wsdl.ComplexType) {
	if ct.ComplexContent == nil || ct.ComplexContent.Extension == nil || ct.TargetNamespace == "" {
		return
	}

	ext := ct.ComplexContent.Extension
	if ext.Base != "" && !ct.Abstract {
		ge.writeComments(w, "SetXMLType", "")
		fmt.Fprintf(w, "func (t *%s) SetXMLType() {\n", goSymbol(ct.Name))
		fmt.Fprintf(w, "if t.OverrideTypeNamespace != nil {\n")
		fmt.Fprintf(w, "    t.TypeNamespace = *t.OverrideTypeNamespace\n")
		fmt.Fprintf(w, "} else {\n")
		fmt.Fprintf(w, "    t.TypeNamespace = \"%s\"\n", ct.TargetNamespace)
		fmt.Fprintf(w, "}\n}\n\n")
	}
}

// helper function to print out the XMLName
func (ge *goEncoder) genXMLName(w io.Writer, targetNamespace string, name string) {
	if elName, ok := ge.needsTag[name]; ok {
		if ge.localNamespace == "" {
			fmt.Fprintf(w, "XMLName xml.Name `xml:\"%s %s\" json:\"-\" yaml:\"-\"`\n",
				targetNamespace, elName)
		} else {
			fmt.Fprintf(w, "XMLName xml.Name `xml:\"%s:%s\" json:\"-\" yaml:\"-\"`\n",
				ge.localNamespace, elName)
		}
	}
}

var invalidGoSymbol = regexp.MustCompile(`[0-9_]*[^0-9a-zA-Z_]+`)

func goSymbol(s string) string {
	v := invalidGoSymbol.ReplaceAllString(trimns(s), " ")
	var name string
	for _, part := range strings.Split(v, " ") {
		name += strings.Title(part)
	}
	return name
}

func trimns(s string) string {
	n := strings.SplitN(s, ":", 2)
	if len(n) == 2 {
		return n[1]
	}
	return s
}

func (ge *goEncoder) genGoStruct(w io.Writer, d *wsdl.Definitions, ct *wsdl.ComplexType) error {
	c := 0
	if len(ct.AllElements) == 0 {
		c++
	}
	if ct.ComplexContent == nil || ct.ComplexContent.Extension == nil {
		c++
	}
	if ct.Sequence == nil && ct.Choice == nil {
		c++
	} else if ct.Sequence != nil &&
		(len(ct.Sequence.ComplexTypes) == 0 && len(ct.Sequence.Elements) == 0 && len(ct.Sequence.Choices) == 0) {
		c++
	} else if ct.Choice != nil && (len(ct.Choice.ComplexTypes) == 0 && len(ct.Choice.Elements) == 0 && (ct.Choice.Sequence == nil || len(ct.Choice.Sequence.Elements) == 0)) {
		c++
	}

	name := goSymbol(ct.Name)
	ge.writeComments(w, name, ct.Doc)
	if ct.Abstract {
		fmt.Fprintf(w, "type %s interface{}\n\n", name)
		return nil
	}
	if ct.Sequence != nil && ct.Sequence.Any != nil {
		if len(ct.Sequence.Elements) == 0 {
			fmt.Fprintf(w, "type %s []interface{}\n\n", name)
			return nil
		}
	}
	if ct.Choice != nil && ct.Choice.Any != nil {
		if len(ct.Choice.Elements) == 0 {
			fmt.Fprintf(w, "type %s []interface{}\n\n", name)
			return nil
		}
	}
	if ct.ComplexContent != nil {
		restr := ct.ComplexContent.Restriction
		if restr != nil && len(restr.Attributes) == 1 && restr.Attributes[0].ArrayType != "" {
			fmt.Fprintf(w, "type %s struct {\n", name)
			typ := strings.SplitN(trimns(restr.Attributes[0].ArrayType), "[", 2)[0]
			fmt.Fprintf(w, "Items []%s `xml:\"item,omitempty\" json:\"item,omitempty\" yaml:\"item,omitempty\"`\n", ge.wsdl2goType(typ))
			fmt.Fprintf(w, "}\n\n")
			return nil
		}
	}

	if c > 2 && len(ct.Attributes) == 0 && ct.SimpleContent == nil {
		fmt.Fprintf(w, "type %s struct {\n", name)
		ge.genXMLName(w, d.TargetNamespace, name)
		fmt.Fprintf(w, "}\n\n")
		return nil
	}
	fmt.Fprintf(w, "type %s struct {\n", name)
	ge.genXMLName(w, d.TargetNamespace, name)

	err := ge.genStructFields(w, d, ct, RedefinedStructFields{})

	if (ct.ComplexContent != nil && ct.ComplexContent.Extension != nil) ||
		(ct.Choice != nil && ct.Choice.Sequence != nil) ||
		(ct.Sequence != nil) {

		fmt.Fprint(w, "TypeNamespace string `xml:\"xmlns,attr,omitempty\"`\n")
		fmt.Fprint(w, "\n")
		fmt.Fprint(w, "OverrideTypeNamespace *string `xml:\"-\"`\n")
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "}\n\n")
	return nil
}

func (ge *goEncoder) genGoOpStruct(w io.Writer, d *wsdl.Definitions, bo *wsdl.BindingOperation) error {
	name := goSymbol(bo.Name)

	inputMessage := ge.messages[trimns(ge.funcs[bo.Name].Input.Message)]

	// No-Op on operations which don't take arguments
	// (These can be inlined, and don't need to pollute the file)
	if len(inputMessage.Parts) > 0 {
		ge.genOpStructMessage(w, d, name, inputMessage)
	}

	// Output messages are always required
	ge.genOpStructMessage(w, d, name, ge.messages[trimns(ge.funcs[bo.Name].Output.Message)])

	return nil
}

func (ge *goEncoder) genOpStructMessage(w io.Writer, d *wsdl.Definitions, name string, message *wsdl.Message) {
	sanitizedMessageName := ge.sanitizedOperationsType(message.Name)

	if ge.wroteOpStruct[sanitizedMessageName] {
		return
	}
	ge.wroteOpStruct[sanitizedMessageName] = true

	ge.writeComments(w, sanitizedMessageName, "Operation wrapper for "+name+".")
	ge.writeComments(w, sanitizedMessageName, "")
	fmt.Fprintf(w, "type %s struct {\n", sanitizedMessageName)
	if elName, ok := ge.needsTag[sanitizedMessageName]; ok {
		fmt.Fprintf(w, "XMLName xml.Name `xml:\"%s %s\" json:\"-\" yaml:\"-\"`\n",
			d.TargetNamespace, elName)
	}

	for _, part := range message.Parts {
		wsdlType := part.Type

		// Probably soap12
		if wsdlType == "" {
			wsdlType = part.Element
		}

		partName := part.Name
		if part.Element != "" {
			elName := trimns(part.Element)
			if el, ok := ge.elements[elName]; ok {
				partName = trimns(el.Name)
			} else if el, ok := ge.ctypes[elName]; ok {
				partName = trimns(el.Name)
			} else if el, ok := ge.stypes[elName]; ok {
				partName = trimns(el.Name)
			}
		}

		ge.genElementField(w, &wsdl.Element{
			XMLName: part.XMLName,
			Name:    partName,
			Type:    wsdlType,
			// TODO: Maybe one could make guesses about nillable?
		}, RedefinedStructFields{})
	}

	fmt.Fprintf(w, "}\n\n")
}

type RedefinedStructFields map[string]bool

func (ge *goEncoder) genStructFields(w io.Writer, d *wsdl.Definitions, ct *wsdl.ComplexType, redefined RedefinedStructFields) error {
	for _, el := range ct.AllElements {
		ge.genElementField(w, el, redefined)
	}
	if ct.Sequence != nil {
		for _, el := range ct.Sequence.Elements {
			ge.genElementField(w, el, redefined)
		}
		for _, choice := range ct.Sequence.Choices {
			for _, el := range choice.Elements {
				ge.genElementField(w, el, redefined)
			}
		}
	}
	if ct.Choice != nil {
		for _, el := range ct.Choice.Elements {
			ge.genElementField(w, el, redefined)
		}

		if ct.Choice.Sequence != nil {
			for _, el := range ct.Choice.Sequence.Elements {
				ge.genElementField(w, el, redefined)
			}
		}
	}
	for _, attr := range ct.Attributes {
		ge.genAttributeField(w, attr, redefined)
	}

	if ge.requestVersion != "" {
		attr := &wsdl.Attribute{
			XMLName:  xml.Name{Space: "http://www.w3.org/2001/XMLSchema", Local: "attribute"},
			Name:     "Version",
			Type:     "xsd:string",
			Nillable: false,
		}
		ge.genAttributeField(w, attr, redefined)
	}

	return nil
}

func (ge *goEncoder) genElementField(w io.Writer, el *wsdl.Element, redefined RedefinedStructFields) {
	if redefined[el.Name] {
		return
	}
	redefined[el.Name] = true

	if el.Ref != "" {
		ref := trimns(el.Ref)
		refElement, ok := ge.elements[ref]
		if !ok {
			return
		}
		el = refElement.Copy().Enrich(el)
	}
	var slicetype string
	if el.Type == "" && el.ComplexType != nil {
		seq := el.ComplexType.Sequence
		if seq == nil && el.ComplexType.Choice != nil {
			seq = &wsdl.Sequence{
				ComplexTypes: el.ComplexType.Choice.ComplexTypes,
				Elements:     el.ComplexType.Choice.Elements,
				Any:          el.ComplexType.Choice.Any}
		}
		if seq != nil {
			if len(seq.Elements) == 1 {
				n := el.Name
				seqel := seq.Elements[0]
				el = new(wsdl.Element)
				*el = *seqel
				slicetype = seqel.Name
				el.Name = n
			} else if len(seq.Any) == 1 {
				el = &wsdl.Element{
					Name: el.Name,
					Type: "anysequence",
					Min:  seq.Any[0].Min,
					Max:  seq.Any[0].Max,
				}
				slicetype = el.Name
			}
		}
	}

	et := el.Type
	if et == "" {
		et = "string"
	}

	tag := el.Name
	fmt.Fprintf(w, "%s ", goSymbol(el.Name))
	if el.Max != "" && el.Max != "1" {
		fmt.Fprintf(w, "[]")
		if slicetype != "" {
			tag = el.Name + ">" + slicetype
		}
	}

	typ := ge.wsdl2goType(et)
	if el.Nillable || el.Min == 0 {

		tag += ",omitempty"
		//since we add omitempty tag, we should add pointer to type.
		//thus xmlencoder can differ not-initialized fields from zero-initialized values
		if !ge.noSimpleTypeIndirect && !strings.HasPrefix(typ, "*") {
			typ = "*" + typ
		}
	}
	if el.Tag != "" {
		tag = el.Tag
	}

	fmt.Fprintf(w, "%s `xml:\"%s\" json:\"%s\" yaml:\"%s\"`\n",
		typ, tag, tag, tag)
}

func (ge *goEncoder) genAttributeField(w io.Writer, attr *wsdl.Attribute, redefined RedefinedStructFields) {
	if redefined[attr.Name] {
		return
	}
	redefined[attr.Name] = true

	if attr.Name == "" && attr.Ref != "" {
		attr.Name = trimns(attr.Ref)
	}
	if attr.Type == "" {
		attr.Type = "string"
	}

	tagName := attr.TagName
	if tagName == "" {
		tagName = attr.Name
	}

	tag := fmt.Sprintf("%s,attr", tagName)
	fmt.Fprintf(w, "%s ", goSymbol(attr.Name))
	typ := ge.wsdl2goType(attr.Type)
	if attr.Nillable || attr.Min == 0 {
		tag += ",omitempty"
	}
	fmt.Fprintf(w, "%s `xml:\"%s\" json:\"%s\" yaml:\"%s\"`\n",
		typ, tag, tag, tag)
}

// writeComments writes comments to w, capped at ~80 columns.
func (ge *goEncoder) writeComments(w io.Writer, typeName, comment string) {
	comment = strings.Trim(strings.Replace(comment, "\n", " ", -1), " ")
	if comment == "" {
		comment = goSymbol(typeName) + " was auto-generated from WSDL."
	}
	count, line := 0, ""
	words := strings.Split(comment, " ")
	for _, word := range words {
		if line == "" {
			count, line = 2, "//"
		}

		count += len(word)
		if count > 60 {
			fmt.Fprintf(w, "%s %s\n", line, word)
			count, line = 0, ""
			continue
		}
		line = line + " " + word
		count++
	}
	if line != "" {
		fmt.Fprintf(w, "%s\n", line)
	}
	return
}

// SetLocalNamespace allows overridding of namespace in XMLName
func (ge *goEncoder) SetLocalNamespace(s string) {
	ge.localNamespace = s
}
