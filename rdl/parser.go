// Copyright 2015 Yahoo Inc.
// Licensed under the terms of the Apache version 2.0 license. See LICENSE file for terms.

package rdl

import (
	"bufio"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"text/scanner"
	"unicode"
)

type parser struct {
	schema         *Schema
	parent         *parser
	scanner        *scanner.Scanner
	err            error
	registry       *typeRegistry
	included       map[string]bool
	legacySynonyms map[string]string
	types          []string
	resources      []*Resource
	verbose        bool
	pedantic       bool
	nowarn         bool
}

func (p *parser) String() string {
	return "<scanner " + p.scanner.Filename + ">"
}

// ParseRDLFile parses the specified file to produce a Schema object.
func ParseRDLFile(path string, verbose bool, pedantic bool, nowarn bool) (*Schema, error) {
	return parseRDLFile(path, nil, verbose, pedantic, nowarn)
}

func parseRDLFile(path string, parent *parser, verbose bool, pedantic bool, nowarn bool) (*Schema, error) {
	fi, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fi.Close()
	reader := bufio.NewReader(fi)
	return parseRDL(parent, path, reader, verbose, pedantic, nowarn)
}

func isIdentRune(ch rune, i int) bool {
	return ch == '_' || unicode.IsLetter(ch) || unicode.IsDigit(ch) && i > 0
}

func parseRDL(parent *parser, source string, reader io.Reader, verbose bool, pedantic bool, nowarn bool) (*Schema, error) {
	p := new(parser)
	p.legacySynonyms = map[string]string{
		"byte":    "Int8",
		"short":   "Int16",
		"integer": "Int32",
		"long":    "Int64",
		"float":   "Float32",
		"double":  "Float64",
		"boolean": "Bool",
	}
	p.parent = parent
	p.verbose = verbose
	p.pedantic = pedantic
	p.nowarn = nowarn
	p.scanner = new(scanner.Scanner)
	p.scanner.Init(reader)
	p.scanner.Filename = source
	p.scanner.Mode = scanner.ScanComments | scanner.ScanIdents | scanner.ScanStrings | scanner.ScanFloats
	p.scanner.Error = func(s *scanner.Scanner, msg string) { p.error(msg) }
	p.scanner.IsIdentRune = isIdentRune //Only works with go1.4 and later.
	p.schema = NewSchema()
	p.registry = newTypeRegistry(p.schema)
	p.parseSchema()
	return p.schema, p.err
}

func max(n1 int, n2 int) int {
	if n1 > n2 {
		return n1
	}
	return n2
}

func min(n1 int, n2 int) int {
	if n1 < n2 {
		return n1
	}
	return n2
}

func (p *parser) warning(msg string) {
	if !p.nowarn {
		s := p.formattedAnnotation(p.scanner.Pos(), msg, true)
		fmt.Fprintln(os.Stderr, s)
	}
}

func (p *parser) error(msg string) {
	s := p.formattedAnnotation(p.scanner.Pos(), msg, false)
	p.err = fmt.Errorf(s)
}

func (p *parser) formattedAnnotation(pos scanner.Position, msg string, warning bool) string {
	prefix := "Error"
	if warning {
		prefix = "Warning"
	}
	if len(pos.Filename) > 0 {
		if p.verbose {
			red := "\033[0;31m"
			yellow := "\033[0;33m"
			black := "\033[0;0m"
			color := red
			if warning {
				color = yellow
			}
			data, err := ioutil.ReadFile(pos.Filename)
			if err == nil {
				lines := strings.Split(string(data), "\n")
				line := pos.Line - 1
				begin := max(0, line-10)
				end := min(len(lines), line+10)
				context := lines[begin:end]
				tmp := ""
				for i, l := range context {
					if i+begin == line {
						tmp += fmt.Sprintf("%s%3d\t%v\n%s", color, i+begin+1, l, black)
					} else {
						tmp += fmt.Sprintf("%3d\t%v\n", i+begin+1, l)
					}
				}
				return fmt.Sprintf("%s%s (%s, line %d): %s%s\n%s", color, prefix, path.Base(pos.Filename), pos.Line, msg, black, tmp)
			}
		}
		return fmt.Sprintf("%s(%s:%d): %s", prefix, filepath.Base(pos.Filename), pos.Line, msg)
	}
	return fmt.Sprintf("%s(line %d): %s", prefix, pos.Line, msg)
}

func (p *parser) expectedError(expected string) {
	p.error(fmt.Sprintf("expected %s, found '%s'", expected, p.scanner.TokenText()))
}

func (p *parser) trailingComment(prev string) string {
	//check for trailing line comment, adds to the prev if present
	c := p.scanner.Peek()
	for c != '\n' && p.isWhitespace(c) {
		p.scanner.Next()
		c = p.scanner.Peek()
		if c == '\n' {
			break
		}
	}
	if c == '/' {
		tok := p.scanner.Scan()
		comment, _ := p.parseComment(tok, prev)
		return comment
	}
	return prev
}

func (p *parser) parseSchema() {
	tok := p.scanner.Scan()
	comment := ""
	for tok != scanner.EOF && p.err == nil {
		txt := p.scanner.TokenText()
		switch tok {
		case scanner.Comment:
			//accumulate comments, building a single block comment
			comment, _ = p.parseComment(tok, comment)
		case scanner.Ident:
			switch txt {
			case "namespace":
				p.schema.Comment = p.mergeComment(p.schema.Comment, comment)
				comment = ""
				p.parseNamespace()
			case "name", "service":
				if txt == "service" && !p.acceptLegacy("'service'", "use 'name', not 'service'") {
					return
				}
				p.schema.Comment = p.mergeComment(p.schema.Comment, comment)
				comment = ""
				p.parseName()
			case "version":
				p.schema.Comment = p.mergeComment(p.schema.Comment, comment)
				comment = ""
				p.parseVersion()
			case "include":
				p.schema.Comment = p.mergeComment(p.schema.Comment, comment)
				comment = ""
				p.parseInclude()
			case "use":
				p.schema.Comment = p.mergeComment(p.schema.Comment, comment)
				comment = ""
				p.parseUse()
			case "type":
				typeComment := comment
				comment = ""
				t := p.parseType(typeComment)
				if t != nil {
					p.registerType(t)
				}
			case "resource":
				resourceComment := comment
				comment = ""
				r := p.parseResource(resourceComment)
				if r != nil {
					p.registerResource(r)
				}
			default:
				p.error("Unrecognized keyword in schema: '" + txt + "'")
			}
		case ';':
			p.warning("stray ';' character")
		case '#':
			if !p.acceptLegacy("'#' for line comments, use '//' instead", "use '//', not '#'") {
				return
			}
			comment = p.parseLegacyComment(comment)
		default:
			p.error("unexpected token")
		}
		if p.err != nil {
			return
		}
		tok = p.scanner.Scan()

	}
	if p.types != nil {
		types := make([]*Type, 0, len(p.types))
		for _, typeName := range p.types {
			t := p.findType(TypeRef(typeName))
			types = append(types, t)
		}
		p.schema.Types = types
	}
	if p.resources != nil {
		p.schema.Resources = p.resources
	}
}

func (p *parser) parseExtendedOption(options map[ExtendedAnnotation]string, optname ExtendedAnnotation) map[ExtendedAnnotation]string {
	c := p.skipWhitespaceExceptNewline()
	optval := ""
	if c == '=' {
		p.scanner.Next()
		optval = p.stringLiteral("String literal")
		if p.err != nil {
			return nil
		}
	}
	if options == nil {
		options = make(map[ExtendedAnnotation]string)
	}
	options[optname] = optval
	return options
}

func (p *parser) parseNamespace() {
	if p.err != nil {
		return
	}
	if p.schema.Namespace != "" {
		p.error("duplicate namespace declaration")
	} else {
		//the default go scanner won't return compound names as identifiers, and we would lose track of
		//the trailing newline (if semicolon was omitted). So, we brute force the parse of the dotted name
		var buf []rune
		c := p.skipWhitespaceExceptNewline()
		if isIdentRune(c, 1) {
			for c == '.' || isIdentRune(c, 1) {
				buf = append(buf, c)
				p.scanner.Next()
				c = p.scanner.Peek()
			}
			p.skipWhitespaceExceptNewline()
			ns := strings.Trim(string(buf), " ")
			if ns != "" {
				p.schema.Namespace = NamespacedIdentifier(ns)
			}
			p.schema.Comment = p.statementEnd(p.schema.Comment)
		} else {
			p.expectedError("dotted name")
		}
	}
}

func (p *parser) parseName() {
	if p.err != nil {
		return
	}
	if p.schema.Name != "" {
		p.error("duplicate name declaration")
	} else {
		n := p.identifier("name")
		p.schema.Comment = p.statementEnd(p.schema.Comment)
		if p.err == nil {
			p.schema.Name = Identifier(n)
		}
	}
}

func (p *parser) parseVersion() {
	if p.err != nil {
		return
	}
	if p.schema.Version != nil {
		p.error("duplicate version declaration")
	} else {
		n := p.int32Literal("integer value")
		p.schema.Comment = p.statementEnd(p.schema.Comment)
		if p.err == nil {
			p.schema.Version = &n
		}
	}
}

func (p *parser) includedFile(filename string) bool {
	if p.parent != nil {
		return p.parent.includedFile(filename)
	}
	if p.included != nil {
		if _, ok := p.included[filename]; ok {
			return true
		}
	}
	return false
}

func (p *parser) registerIncludedFile(filename string) {
	if p.parent != nil {
		p.parent.registerIncludedFile(filename)
		return
	}
	if p.included == nil {
		p.included = make(map[string]bool)
	}
	p.included[filename] = true
}

func (p *parser) parseInclude() {
	if p.err != nil {
		return
	}
	dir := filepath.Dir(p.scanner.Filename)
	if p.err == nil {
		fname := p.stringLiteral("name of file to include")
		p.schema.Comment = p.statementEnd(p.schema.Comment)
		path := filepath.Join(dir, fname)
		if p.includedFile(path) {
			return
		}
		schema, err := parseRDLFile(path, p, p.verbose, p.pedantic, p.nowarn)
		if err != nil {
			p.err = err
		} else {
			for _, t := range schema.Types {
				p.registerType(t)
			}
			for _, rez := range schema.Resources {
				p.registerResource(rez)
			}
			p.registerIncludedFile(path)
		}
	}
}

//prefix the type (and its constituents) with the given prefix, used when "use"ing another schema.
//i.e. use "other.rdl" where other.rdl contains a Foo type will prefix the type "other.Foo" within the
//type that is using it.
func (p *parser) prefixType(reg TypeRegistry, t *Type, prefix string) *Type {
	pre := TypeName(prefix)
	switch t.Variant {
	case TypeVariantAliasTypeDef:
		t.AliasTypeDef.Name = pre + t.AliasTypeDef.Name
		if !reg.IsBaseTypeName(t.AliasTypeDef.Type) {
			t.AliasTypeDef.Type = TypeRef(pre) + t.AliasTypeDef.Type
		}
	case TypeVariantStringTypeDef:
		t.StringTypeDef.Name = pre + t.StringTypeDef.Name
		if !reg.IsBaseTypeName(t.StringTypeDef.Type) {
			t.StringTypeDef.Type = TypeRef(pre) + t.StringTypeDef.Type
		}
	case TypeVariantNumberTypeDef:
		t.NumberTypeDef.Name = pre + t.NumberTypeDef.Name
		if !reg.IsBaseTypeName(t.NumberTypeDef.Type) {
			t.NumberTypeDef.Type = TypeRef(pre) + t.NumberTypeDef.Type
		}
	case TypeVariantArrayTypeDef:
		t.ArrayTypeDef.Name = pre + t.ArrayTypeDef.Name
		if !reg.IsBaseTypeName(t.ArrayTypeDef.Type) {
			t.ArrayTypeDef.Type = TypeRef(pre) + t.ArrayTypeDef.Type
		}
		if !reg.IsBaseTypeName(t.ArrayTypeDef.Items) {
			t.ArrayTypeDef.Items = TypeRef(pre) + t.ArrayTypeDef.Items
		}
	case TypeVariantMapTypeDef:
		t.MapTypeDef.Name = pre + t.MapTypeDef.Name
		if !reg.IsBaseTypeName(t.MapTypeDef.Type) {
			t.MapTypeDef.Type = TypeRef(pre) + t.MapTypeDef.Type
		}
		if t.MapTypeDef.Keys != "" && !reg.IsBaseTypeName(t.MapTypeDef.Keys) {
			t.MapTypeDef.Keys = TypeRef(pre) + t.MapTypeDef.Keys
		}
		if t.MapTypeDef.Items != "" && !reg.IsBaseTypeName(t.MapTypeDef.Items) {
			t.MapTypeDef.Items = TypeRef(pre) + t.MapTypeDef.Items
		}
	case TypeVariantStructTypeDef:
		t.StructTypeDef.Name = pre + t.StructTypeDef.Name
		if !reg.IsBaseTypeName(t.StructTypeDef.Type) {
			t.StructTypeDef.Type = TypeRef(pre) + t.StructTypeDef.Type
		}
		for _, f := range t.StructTypeDef.Fields {
			if !reg.IsBaseTypeName(f.Type) {
				f.Type = TypeRef(pre) + f.Type
			}
			if f.Keys != "" && !reg.IsBaseTypeName(f.Keys) {
				f.Keys = TypeRef(pre) + f.Keys
			}
			if f.Items != "" && !reg.IsBaseTypeName(f.Items) {
				f.Items = TypeRef(pre) + f.Items
			}
		}
	case TypeVariantBytesTypeDef:
		t.BytesTypeDef.Name = pre + t.BytesTypeDef.Name
		if !reg.IsBaseTypeName(t.BytesTypeDef.Type) {
			t.BytesTypeDef.Type = TypeRef(pre) + t.BytesTypeDef.Type
		}
	case TypeVariantEnumTypeDef:
		t.EnumTypeDef.Name = pre + t.EnumTypeDef.Name
		if !reg.IsBaseTypeName(t.EnumTypeDef.Type) {
			t.EnumTypeDef.Type = TypeRef(pre) + t.EnumTypeDef.Type
		}
	case TypeVariantUnionTypeDef:
		t.UnionTypeDef.Name = pre + t.UnionTypeDef.Name
		if !reg.IsBaseTypeName(t.UnionTypeDef.Type) {
			t.UnionTypeDef.Type = TypeRef(pre) + t.UnionTypeDef.Type
		}
		for i := 0; i < len(t.UnionTypeDef.Variants); i++ {
			if !reg.IsBaseTypeName(t.UnionTypeDef.Variants[i]) {
				t.UnionTypeDef.Variants[i] = TypeRef(pre) + t.UnionTypeDef.Variants[i]
			}
		}
	case TypeVariantBaseType:
		return nil
	}
	return t
}

func (p *parser) useType(reg TypeRegistry, t *Type, prefix string) *Type {
	//make sure all its internal types are also prefixed
	tt := p.prefixType(reg, t, prefix)
	if tt != nil {
		p.registerType(tt)
	}
	return tt
}

func (p *parser) parseUse() {
	if p.err != nil {
		return
	}
	dir := filepath.Dir(p.scanner.Filename)
	if p.err == nil {
		fname := p.stringLiteral("name of file to use")
		p.schema.Comment = p.statementEnd(p.schema.Comment)
		var schema *Schema
		var err error
		path := fname
		if fname == "rdl" {
			if p.includedFile(path) {
				return
			}
			schema = RdlSchema()
		} else {
			path = filepath.Join(dir, fname)
			if p.includedFile(path) {
				return
			}
			schema, err = parseRDLFile(path, p, p.verbose, p.pedantic, p.nowarn)
		}
		if err != nil {
			p.err = err
		} else {
			prefix := string(schema.Name + ".")
			for _, t := range schema.Types {
				p.useType(p.registry, t, prefix)
			}
			p.registerIncludedFile(path)
		}
	}
}

func (p *parser) statementEnd(comment string) string {
	c := p.skipWhitespaceExceptNewline()
	if c == ';' {
		p.scanner.Next()
		c = p.skipWhitespaceExceptNewline()
	}
	if c == '/' {
		comment = p.trailingComment(comment)
	}
	return comment
}

func (p *parser) expect(expected string) bool {
	if p.err == nil {
		_ = p.scanner.Scan()
		txt := p.scanner.TokenText()
		if txt != expected {
			p.expectedError("'" + expected + "'")
			return false
		}
		return true
	}
	return false
}

func (p *parser) identifier(expected string) Identifier {
	if p.err == nil {
		tok := p.scanner.Scan()
		if tok == scanner.Ident {
			return Identifier(p.scanner.TokenText())
		}
		p.expectedError("'" + expected + "'")
	}
	return ""
}

func (p *parser) stringLiteral(expected string) string {
	if p.err == nil {
		tok := p.scanner.Scan()
		if tok == scanner.String {
			s := p.scanner.TokenText()
			q, err := strconv.Unquote(s)
			if err != nil {
				p.error("Improperly escaped string: " + s)
				return s
			}
			return q
		}
		p.expectedError("'" + expected + "'")
	}
	return ""
}

func (p *parser) numericLiteral(expected string) float64 {
	if p.err == nil {
		tok := p.scanner.Scan()
		if tok == scanner.Int {
			n, err := strconv.ParseInt(p.scanner.TokenText(), 10, 64)
			if err == nil {
				return float64(n)
			}
		} else if tok == scanner.Float {
			n, err := strconv.ParseFloat(p.scanner.TokenText(), 64)
			if err == nil {
				return n
			}
		} else if tok == '-' {
			f := p.numericLiteral(expected)
			return -f
		}
		p.expectedError(expected)
	}
	return 0
}

func (p *parser) int32Literal(expected string) int32 {
	if p.err == nil {
		tok := p.scanner.Scan()
		if tok == scanner.Int {
			n, err := strconv.Atoi(p.scanner.TokenText())
			if err == nil {
				return int32(n)
			}
		}
		p.expectedError(expected)
	}
	return 0
}

func (p *parser) findLegacySynonym(context *parser, name string) *Type {
	lowerName := strings.ToLower(name)
	if !p.pedantic {
		if n, ok := p.legacySynonyms[lowerName]; ok {
			if !p.pedantic {
				tt := p.registry.FindType(TypeRef(n))
				if tt != nil {
					tName, _, _ := TypeInfo(tt)
					pp := p
					if context != nil {
						pp = context
					}
					if !pp.nowarn {
						pp.warning("Use '" + string(tName) + "', not '" + name + "'")
					}
					return tt
				}
			}
		}
	}
	return nil
}

const forwardReferenceTag = "___forward_reference___"

func (p *parser) registerType(t *Type) {
	name, _, _ := TypeInfo(t)
	prev := p.findType(TypeRef(name))
	if prev != nil {
		if t.AliasTypeDef != nil && t.AliasTypeDef.Type == forwardReferenceTag {
			if !p.nowarn {
				p.warning("redefinition of " + string(name))
			}
			return //we already have a def, don't need a forward reference
		}
		if equal(prev, t) {
			//the same, just ignore subsequent defns
			return
		}
		forwardRef := prev.AliasTypeDef != nil && prev.AliasTypeDef.Type == forwardReferenceTag
		if p.pedantic && !forwardRef {
			fmt.Println("prev:", prev)
			fmt.Println("t:", t)
			p.error("conflicting definitions of " + string(name))
		} else {
			idx := -1
			for i, n := range p.types {
				if n == string(name) {
					idx = i
					break
				}
			}
			if idx >= 0 {
				p.types = append(p.types[:idx], p.types[idx+1:]...)
			}
		}
	}
	p.registry.addType(t)
	p.types = append(p.types, string(name))
}

func (p *parser) findType(name TypeRef) *Type {
	return p.findTypeInContext(p, name)
}

func (p *parser) findTypeInContext(context *parser, name TypeRef) *Type {
	t := p.registry.FindType(name)
	if t != nil {
		return t
	}
	if p.parent != nil { //for included schemas, appeal to the schema that includes it
		return p.parent.findTypeInContext(context, name)
	}
	return p.findLegacySynonym(context, string(name))
}

func (p *parser) baseTypeByName(supertypeName TypeRef) BaseType {
	var bt BaseType
	inThisInclude := p.baseType(p.findType(supertypeName))
	if inThisInclude == bt && p.parent != nil {
		return p.parent.baseType(p.findType(supertypeName))
	}
	return inThisInclude
}

func (p *parser) baseType(t *Type) BaseType {
	var bt BaseType
	inThisInclude := p.registry.BaseType(t)
	if inThisInclude == bt && p.parent != nil {
		return p.parent.baseType(t)
	}
	return inThisInclude
}

func (p *parser) parseTypeRef(expected string) TypeRef {
	sym := string(p.identifier(expected))
	if p.err == nil {
		c := p.scanner.Peek()
		if c == '.' {
			p.scanner.Next()
			tok := p.scanner.Scan()
			if tok == scanner.Ident {
				sym = sym + "." + p.scanner.TokenText()
			} else {
				p.error("type reference must be a simple or compound name")
				return TypeRef("")
			}
		}
	}
	return TypeRef(sym)
}

func (p *parser) normalizeTypeName(typeName Identifier, supertypeName TypeRef) (Identifier, TypeRef) {
	prev := p.findType(TypeRef(typeName))
	if prev != nil {
		if prev.Variant == TypeVariantBaseType {
			p.error(fmt.Sprintf("type definition cannot override RDL base type: %v", prev.BaseType))
			return "", ""
		}
	}
	prev = p.findType(TypeRef(supertypeName))
	if prev != nil {
		if prev.Variant == TypeVariantBaseType {
			return typeName, TypeRef(fmt.Sprint(prev.BaseType))
		}
	}
	return typeName, supertypeName
}

func (p *parser) parseType(comment string) *Type {
	var t *Type
	typeName := p.identifier("type name")
	supertypeName := p.parseTypeRef("supertype name")
	typeName, supertypeName = p.normalizeTypeName(typeName, supertypeName)
	if p.err != nil {
		return nil
	}
	tmpDef := NewAliasTypeDef()
	tmpDef.Name = TypeName(typeName)
	tmpDef.Type = forwardReferenceTag
	tmpType := &Type{Variant: TypeVariantAliasTypeDef, AliasTypeDef: tmpDef}
	p.registerType(tmpType) //so recursive references work. This will get replaced.
	if p.err == nil {
		bt := p.baseTypeByName(supertypeName)
		switch bt {
		case BaseTypeStruct:
			t = p.parseStructType(typeName, supertypeName, comment)
		case BaseTypeArray:
			t = p.parseArrayType(typeName, supertypeName, comment)
		case BaseTypeMap:
			t = p.parseMapType(typeName, supertypeName, comment)
		case BaseTypeString, BaseTypeUUID, BaseTypeSymbol, BaseTypeTimestamp:
			t = p.parseStringType(typeName, supertypeName, comment, bt.String())
		case BaseTypeInt8, BaseTypeInt16, BaseTypeInt32, BaseTypeInt64, BaseTypeFloat32, BaseTypeFloat64:
			t = p.parseNumericType(typeName, supertypeName, comment)
		case BaseTypeUnion:
			t = p.parseUnionType(typeName, supertypeName, comment)
		case BaseTypeEnum:
			t = p.parseEnumType(typeName, supertypeName, comment)
		case BaseTypeBool:
			t = p.parseBoolType(typeName, supertypeName, comment)
		case BaseTypeBytes:
			t = p.parseBytesType(typeName, supertypeName, comment)
		default:
			p.error("Cannot derive from this type: " + string(supertypeName))
		}
	}
	return t
}

func (p *parser) isWhitespace(ch rune) bool {
	return p.scanner.Whitespace&(1<<uint(ch)) != 0
}

func (p *parser) mergeComment(comment1 string, comment2 string) string {
	if comment1 != "" {
		if comment2 != "" {
			return comment1 + " " + comment2
		}
		return comment1
	}
	return comment2
}

func (p *parser) parseComment(tok rune, prev string) (string, bool) {
	if tok == scanner.Comment {
		raw := p.scanner.TokenText()
		if strings.HasPrefix(raw, "//") {
			comment := strings.Trim(raw[2:], " ")
			if len(comment) > 0 {
				return p.mergeComment(prev, comment), true
			}
			return prev, true
		} //else a block comment, which we do not preserve
		return prev, true
	}
	return prev, false
}

func (p *parser) parseLegacyComment(prev string) string {
	var buf []rune
	c := p.scanner.Peek()
	for c != '\n' && c != scanner.EOF {
		if buf == nil {
			buf = make([]rune, 0)
		}
		buf = append(buf, c)
		p.scanner.Next()
		c = p.scanner.Peek()
	}
	s := strings.Trim(string(buf), " ")
	if len(s) > 0 {
		if prev != "" {
			s = prev + " " + s
		}
		return s
	}
	return prev
}

func (p *parser) parseStringPatternOption(t *StringTypeDef) {
	p.expect("=")
	pat := p.stringLiteral("regex pattern")
	head := ""
	tail := pat
	i := strings.Index(tail, "{")
	for i >= 0 {
		tmp := tail[i+1:]
		j := strings.Index(tmp, "}")
		if j < 0 {
			p.error("Malformed pattern reference: " + pat)
			return
		}
		refName := tmp[:j]
		rt := p.findType(TypeRef(refName))
		if rt == nil {
			head = head + tail[:i+j+2]
			tail = tail[i+j+2:]
			i = strings.Index(tail, "{")
			continue
		}
		if p.baseType(rt) != BaseTypeString {
			p.error("pattern references non-string type '" + refName + "': " + pat)
			return
		}
		pat := rt.StringTypeDef.Pattern
		if pat == "" {
			p.error("pattern references string type '" + refName + "' which has no pattern")
			return
		}
		head = head + tail[:i] + pat
		tail = tail[i+j+2:]
		i = strings.Index(tail, "{")
	}
	if tail != "" {
		head += tail
	}
	t.Pattern = head
}

func (p *parser) parseStringValuesOption(t *StringTypeDef) {
	p.expect("=")
	if p.err == nil {
		tok := p.scanner.Scan()
		if tok != '[' {
			p.expectedError("array of string literals")
		} else {
			tok := p.scanner.Scan()
			var values []string
			for tok != ']' && tok != scanner.EOF {
				if tok != ',' {
					if tok != scanner.String {
						p.expectedError("array of string literals")
						return
					}
					s := p.scanner.TokenText()
					q, err := strconv.Unquote(s)
					if err != nil {
						p.error("Improperly escaped string: " + s)
						return
					}
					values = append(values, q)
				}
				tok = p.scanner.Scan()
			}
			if len(values) > 0 {
				t.Values = values
			} else {
				p.error("values option must have at least one entry")
			}
		}
	}
}

func (p *parser) skipWhitespace() bool {
	c := p.scanner.Peek()
	for c != scanner.EOF {
		if !p.isWhitespace(c) {
			return true
		}
		p.scanner.Next()
		c = p.scanner.Peek()
	}
	return false
}

func (p *parser) skipWhitespaceExceptNewline() rune {
	c := p.scanner.Peek()
	for c != scanner.EOF {
		if c == '\n' || !p.isWhitespace(c) {
			return c
		}
		p.scanner.Next()
		c = p.scanner.Peek()
	}
	return 0
}

func (p *parser) parseStringType(typeName Identifier, supertypeName TypeRef, comment string, base string) *Type {
	if supertypeName != "String" {
		comment = p.statementEnd(comment)
		return makeAliasType(TypeName(typeName), supertypeName, comment)
	}
	t := NewStringTypeDef()
	t.Name = TypeName(typeName)
	t.Type = TypeRef(supertypeName)
	t.Comment = comment
	p.skipWhitespaceExceptNewline()
	if p.scanner.Peek() == '(' {
		p.scanner.Next()
		tok := p.scanner.Scan()
		commaExpected := false
		for tok != ')' {
			if commaExpected {
				if tok != ',' {
					p.expectedError("',' or ')'")
					return nil
				}
				tok = p.scanner.Scan()
			} else {
				commaExpected = true
			}
			optname := ""
			if tok != scanner.Ident {
				p.expectedError("option name")
				return nil
			}
			optname = p.scanner.TokenText()
			switch strings.ToLower(optname) {
			case "pattern":
				if base == "String" {
					p.parseStringPatternOption(t)
				} else {
					p.error("Unsupported " + base + " option: " + optname)
					return nil
				}
			case "values":
				p.parseStringValuesOption(t)
			case "minsize":
				if base == "String" {
					p.expect("=")
					if p.err == nil {
						val := p.int32Literal("int32 literal")
						t.MinSize = &val
					}
				} else {
					p.error("Unsupported " + base + " option: " + optname)
					return nil
				}
			case "maxsize":
				if base == "String" {
					p.expect("=")
					if p.err == nil {
						val := p.int32Literal("int32 literal")
						t.MaxSize = &val
					}
				} else {
					p.error("Unsupported " + base + " option: " + optname)
					return nil
				}
			default:
				if strings.HasPrefix(optname, "x_") {
					t.Annotations = p.parseExtendedOption(t.Annotations, ExtendedAnnotation(optname))
				} else {
					p.error("Unsupported " + base + " option: " + optname)
					return nil
				}
			}
			if p.err != nil {
				return nil
			}
			tok = p.scanner.Scan()
		}
	}
	t.Comment = p.statementEnd(t.Comment)
	t.Comment = p.statementEnd(t.Comment)
	return &Type{Variant: TypeVariantStringTypeDef, StringTypeDef: t}
}

func (p *parser) parseStructOptions(typename string) map[ExtendedAnnotation]string {
	options := make(map[ExtendedAnnotation]string)
	c := p.skipWhitespaceExceptNewline()
	if c == '(' {
		p.scanner.Next()
		tok := p.scanner.Scan()
		commaExpected := false
		for tok != ')' {
			if commaExpected {
				if tok != ',' {
					p.expectedError("',' or ')'")
					return nil
				}
				tok = p.scanner.Scan()
			} else {
				commaExpected = true
			}
			optname := ""
			if tok != scanner.Ident {
				p.expectedError("option name")
				return nil
			}
			optname = p.scanner.TokenText()
			switch strings.ToLower(optname) {
			case "closed":
				options["closed"] = ""
			default:
				if strings.HasPrefix(optname, "x_") {
					options = p.parseExtendedOption(options, ExtendedAnnotation(optname))
				} else {
					p.error("Unsupported " + typename + " option: " + optname)
					return nil
				}
			}
			if p.err != nil {
				return nil
			}
			tok = p.scanner.Scan()
		}
	}
	if len(options) == 0 {
		return nil
	}
	return options
}

func makeAliasType(typeName TypeName, supertypeName TypeRef, comment string) *Type {
	tmpDef := NewAliasTypeDef()
	tmpDef.Name = typeName
	tmpDef.Type = supertypeName
	tmpDef.Comment = comment
	return &Type{Variant: TypeVariantAliasTypeDef, AliasTypeDef: tmpDef}
}

func (p *parser) parseStructType(typeName Identifier, supertypeName TypeRef, comment string) *Type {
	c := p.skipWhitespaceExceptNewline()
	if c == ';' || c == '/' {
		comment = p.statementEnd(comment)
		return makeAliasType(TypeName(typeName), TypeRef(BaseTypeStruct.String()), comment)
	}
	t := NewStructTypeDef()
	t.Name = TypeName(typeName)
	t.Type = TypeRef(supertypeName)
	t.Comment = comment
	isClosed := false
	if c == '(' {
		options := p.parseStructOptions("Struct")
		if options != nil {
			if _, ok := options["closed"]; ok {
				isClosed = true
				delete(options, "closed")
			}
		}
		if len(options) > 0 {
			t.Annotations = options
		}
	}
	fcomment := ""
	p.expect("{")
	var fields []*StructFieldDef
	tok := p.scanner.Scan()
	for tok != scanner.EOF {
		if tok == '}' {
			break
		} else {
			switch tok {
			case '#':
				if p.pedantic {
					p.error("legacy line comment character '#' not supported. Use '//'")
					return nil
				}
				if !p.nowarn {
					p.warning("use '//' instead of '#'")
				}
				fcomment = p.parseLegacyComment(fcomment)
				tok = p.scanner.Scan()
			case scanner.Comment:
				fcomment, _ = p.parseComment(tok, fcomment)
				tok = p.scanner.Scan()
			case scanner.Ident:
				sym := p.scanner.TokenText()
				if sym == "closed" {
					if !p.nowarn {
						p.warning("use 'type " + string(t.Name) + " Struct (closed) { ... } syntax instead")
					}
					isClosed = true
					fcomment = p.statementEnd(fcomment)
					tok = p.scanner.Scan()
				} else {
					c = p.scanner.Peek()
					if c == '.' {
						p.scanner.Next()
						tok := p.scanner.Scan()
						if tok == scanner.Ident {
							sym = sym + "." + p.scanner.TokenText()
						} else {
							p.error("type reference must be a compound name")
							return nil
						}
					}
					ft := p.findType(TypeRef(sym))
					if ft == nil {
						p.error("No such type: " + sym)
						return nil
					}
					fieldType, _, _ := TypeInfo(ft)
					field := p.parseStructField(t, string(fieldType), fcomment)
					fcomment = ""
					if p.err != nil {
						return nil
					}
					tok = p.scanner.Scan()
					fields = append(fields, field)
				}
			default:
				p.expectedError("type name or option")
				return nil
			}
		}
	}
	if tok == scanner.EOF {
		p.error("Unterminated struct definition")
		return nil
	}
	t.Closed = isClosed
	t.Comment = p.trailingComment(t.Comment)
	if len(fields) > 0 {
		t.Fields = fields
	}
	return &Type{TypeVariantStructTypeDef, nil, t, nil, nil, nil, nil, nil, nil, nil, nil}
}

func (p *parser) parseStructField(t *StructTypeDef, fieldType string, comment string) *StructFieldDef {
	field := NewStructFieldDef()
	tok := p.scanner.Scan()
	optional := false
	if tok == '<' {
		switch strings.ToLower(fieldType) {
		case "array":
			tt := p.typeSpec()
			if p.err != nil {
				return nil
			}
			if tt != nil {
				s, _, _ := TypeInfo(tt)
				field.Items = TypeRef(s)
			}
			p.expect(">")
		case "map":
			tk := p.typeSpec()
			if p.err != nil {
				return nil
			}
			if tk == nil { //"any"
				p.error("Map key types must derive from String or Symbol")
				return nil
			}
			skeys, _, _ := TypeInfo(tk)
			btkeys := p.baseTypeByName(TypeRef(skeys))
			if btkeys != BaseTypeString && btkeys != BaseTypeSymbol {
				p.error("Map key types must derive from String or Symbol")
				return nil
			}
			field.Keys = TypeRef(skeys)
			p.expect(",")
			ti := p.typeSpec()
			if ti != nil {
				if p.err != nil {
					return nil
				}
				sitems, _, _ := TypeInfo(ti)
				field.Items = TypeRef(sitems)
			}
			p.expect(">")
		default:
			p.error("parameterized type only supported for arrays and maps")
			return nil
		}
		tok = p.scanner.Scan()
	}
	if tok == '.' {
		s := p.identifier("type name")
		if p.err != nil {
			return nil
		}
		fieldType = fieldType + "." + string(s)
		tok = p.scanner.Scan()
	}
	field.Type = TypeRef(fieldType)
	if tok == scanner.Ident {
		field.Name = Identifier(p.scanner.TokenText())
		c := p.skipWhitespaceExceptNewline()
		if c == '(' {
			p.scanner.Next()
			tok = p.scanner.Scan()
			commaExpected := false
			for tok != ')' {
				if commaExpected {
					if tok != ',' {
						p.expectedError("','")
						return nil
					}
					tok = p.scanner.Scan()
				} else {
					commaExpected = true
				}
				if tok != scanner.Ident {
					p.error("malformed field option list")
					return nil
				}
				optname := p.scanner.TokenText()
				switch optname {
				case "optional":
					optional = true
				case "default":
					var val interface{}
					p.expect("=")
					ft := p.findType(TypeRef(fieldType))
					bt := p.baseType(ft)
					switch bt {
					//					bt := p.baseTypeByName(fieldType)
					//					switch strings.ToLower(*bt.Name) {
					case BaseTypeString:
						val = p.stringLiteral("String literal")
					case BaseTypeInt8, BaseTypeInt16, BaseTypeInt32, BaseTypeInt64, BaseTypeFloat32, BaseTypeFloat64:
						val = p.numericLiteral(fmt.Sprintf("%v literal", bt))
					case BaseTypeBool:
						s := p.identifier("'true' or 'false'")
						val = "true" == s
					case BaseTypeEnum:
						s := p.identifier("enum symbol")
						val = s
					default:
						p.error(fmt.Sprintf("cannot provide default value for a %v type", bt))
						return nil
					}
					field.Default = val
				default:
					if strings.HasPrefix(optname, "x_") {
						field.Annotations = p.parseExtendedOption(field.Annotations, ExtendedAnnotation(optname))
					} else {
						p.error("unsupported Struct field option: " + optname)
						return nil
					}
				}
				if p.err != nil {
					return nil
				}
				tok = p.scanner.Scan()
			}
		}
		field.Comment = p.statementEnd(comment)
	} else {
		p.expectedError("field name")
	}
	if optional {
		field.Optional = true
	}
	return field
}

func (p *parser) parseArrayType(typeName Identifier, supertypeName TypeRef, comment string) *Type {
	t := NewArrayTypeDef()
	t.Name = TypeName(typeName)
	t.Type = TypeRef(supertypeName)
	t.Comment = comment
	tok := p.scanner.Scan()
	if tok == '<' {
		itemsType := p.typeSpec()
		if itemsType != nil {
			ti, _, _ := TypeInfo(itemsType)
			t.Items = TypeRef(ti)
		}
		p.expect(">")
		tok = p.scanner.Scan()
	}
	if tok == '(' {
		tok = p.scanner.Scan()
		commaExpected := false
		for tok != ')' {
			if commaExpected {
				if tok != ',' {
					p.expectedError("',' or ')'")
					return nil
				}
				tok = p.scanner.Scan()
			} else {
				commaExpected = true
			}
			if tok != scanner.Ident {
				p.error("malformed field option list")
				return nil
			}
			optname := p.scanner.TokenText()
			switch strings.ToLower(optname) {
			case "minsize":
				p.expect("=")
				if p.err == nil {
					val := p.int32Literal("int32 literal")
					t.MinSize = &val
				}
			case "maxsize":
				p.expect("=")
				if p.err == nil {
					val := p.int32Literal("int32 literal")
					t.MaxSize = &val
				}
			case "size":
				p.expect("=")
				if p.err == nil {
					val := p.int32Literal("int32 literal")
					t.Size = &val
				}
			default:
				if strings.HasPrefix(optname, "x_") {
					t.Annotations = p.parseExtendedOption(t.Annotations, ExtendedAnnotation(optname))
				} else {
					p.error("unsupported Array option: " + optname)
					return nil
				}
			}
			if p.err != nil {
				return nil
			}
			tok = p.scanner.Scan()
		}
	}
	t.Comment = p.statementEnd(t.Comment)
	return &Type{Variant: TypeVariantArrayTypeDef, ArrayTypeDef: t}
}

func (p *parser) parseBytesType(typeName Identifier, supertypeName TypeRef, comment string) *Type {
	t := NewBytesTypeDef()
	t.Name = TypeName(typeName)
	t.Type = TypeRef(supertypeName)
	t.Comment = comment
	tok := p.scanner.Scan()
	if tok == '[' {
		size := p.int32Literal("byte array size, non-negative integer")
		if size < 0 {
			p.error("byte array size, non-negative integer")
			return nil
		}
		p.expect("]")
		if p.err != nil {
			return nil
		}
		t.Size = &size
		tok = p.scanner.Scan()
	}
	if tok == '(' {
		tok = p.scanner.Scan()
		commaExpected := false
		for tok != ')' {
			if commaExpected {
				if tok != ',' {
					p.expectedError("',' or ')'")
					return nil
				}
				tok = p.scanner.Scan()
			} else {
				commaExpected = true
			}
			if tok != scanner.Ident {
				p.error("malformed option list")
				return nil
			}
			optname := p.scanner.TokenText()
			switch strings.ToLower(optname) {
			case "minsize":
				p.expect("=")
				if p.err == nil {
					val := p.int32Literal("int32 literal")
					t.MinSize = &val
				}
			case "maxsize":
				p.expect("=")
				if p.err == nil {
					val := p.int32Literal("int32 literal")
					t.MaxSize = &val
				}
			case "size":
				p.expect("=")
				if p.err == nil {
					val := p.int32Literal("int32 literal")
					t.Size = &val
				}
			default:
				if strings.HasPrefix(optname, "x_") {
					t.Annotations = p.parseExtendedOption(t.Annotations, ExtendedAnnotation(optname))
				} else {
					p.error("unsupported Array option: " + optname)
					return nil
				}
			}
			if t.Size != nil && (t.MaxSize != nil || t.MinSize != nil) {
				p.error("Cannot specify fixed size and minsize/maxsize in the same type")
				return nil
			}
			if p.err != nil {
				return nil
			}
			tok = p.scanner.Scan()
		}
	}
	t.Comment = p.statementEnd(t.Comment)
	return &Type{Variant: TypeVariantBytesTypeDef, BytesTypeDef: t}
}

//in the RDL grammar, a typeref is not a typespec, i.e. it must be a type name or "any".
//to handle nested parameterized types, the grammar needs to be extended. This implies
//that some types will be anonymous. We require names, so type names are generated with
// a name spelling out the items/keys. If the returned typespec is nil, that means an error,
//not "any".
func (p *parser) typeSpec() *Type {
	if p.err == nil {
		tname := string(p.identifier("type name"))
		if strings.ToLower(tname) == "any" {
			return nil
		}
		pType := p.findType(TypeRef(tname))
		if pType == nil {
			p.error("Undefined type: " + tname)
			return nil
		}
		tName, _, _ := TypeInfo(pType)
		c := p.skipWhitespaceExceptNewline()
		if c == '\n' || c == '/' {
			p.error("unexpected end of line")
		}
		if c == '<' {
			p.scanner.Next()
			switch tName {
			case "Array":
				pItems := p.typeSpec()
				p.expect(">")
				if pItems != nil && p.err == nil {
					items, _, _ := TypeInfo(pItems)
					genName := "ArrayOf" + items
					t := p.findType(TypeRef(genName))
					if t != nil {
						return t
					}
					atype := NewArrayTypeDef()
					atype.Name = TypeName(genName)
					atype.Type = "Array"
					atype.Items = TypeRef(items)
					ttt := &Type{Variant: TypeVariantArrayTypeDef, ArrayTypeDef: atype}
					p.registerType(ttt)
					return ttt
				}
			case "Map":
				pKeys := p.typeSpec()
				p.expect(",")
				pItems := p.typeSpec()
				p.expect(">")
				if pItems != nil && pKeys != nil && p.err == nil {
					items, _, _ := TypeInfo(pItems)
					keys, _, _ := TypeInfo(pKeys)
					genName := "MapFrom" + keys + "To" + items
					t := p.findType(TypeRef(genName))
					if t != nil {
						return t
					}
					mtype := NewMapTypeDef()
					mtype.Name = TypeName(genName)
					mtype.Type = "Map"
					mtype.Keys = TypeRef(keys)
					mtype.Items = TypeRef(items)
					mmm := &Type{Variant: TypeVariantMapTypeDef, MapTypeDef: mtype}
					p.registerType(mmm)
					return mmm
				}
			}
		}
		return pType
	}
	return nil
}

func (p *parser) parseMapType(typeName Identifier, supertypeName TypeRef, comment string) *Type {
	t := NewMapTypeDef()
	t.Name = TypeName(typeName)
	t.Type = TypeRef(supertypeName)
	t.Comment = comment
	tok := p.scanner.Scan()
	if tok == '<' {
		tt := p.typeSpec()
		if p.err != nil {
			return nil
		}
		if tt == nil { //Any
			p.error("Map key types must derive from String")
			return nil
		}
		if p.baseType(tt) != BaseTypeString {
			p.error("Map key types must derive from String")
			return nil
		}
		tk, _, _ := TypeInfo(tt)
		t.Keys = TypeRef(tk)
		p.expect(",")
		if p.err != nil {
			return nil
		}
		tt = p.typeSpec()
		if p.err != nil {
			return nil
		}
		if tt != nil {
			ti, _, _ := TypeInfo(tt)
			t.Items = TypeRef(ti)
		}
		p.expect(">")
		if p.err != nil {
			return nil
		}
		tok = p.scanner.Scan()
	}
	if tok == '(' {
		tok = p.scanner.Scan()
		commaExpected := false
		for tok != ')' {
			if commaExpected {
				if tok != ',' {
					p.expectedError("',' or ')'")
					return nil
				}
				tok = p.scanner.Scan()
			} else {
				commaExpected = true
			}
			if tok != scanner.Ident {
				p.error("malformed field option list")
				return nil
			}
			optname := p.scanner.TokenText()
			switch strings.ToLower(optname) {
			case "minsize":
				p.expect("=")
				if p.err == nil {
					val := p.int32Literal("int32 literal")
					t.MinSize = &val
				}
			case "maxsize":
				p.expect("=")
				if p.err == nil {
					val := p.int32Literal("int32 literal")
					t.MaxSize = &val
				}
			case "size":
				p.expect("=")
				if p.err == nil {
					val := p.int32Literal("int32 literal")
					t.Size = &val
				}
			default:
				if strings.HasPrefix(optname, "x_") {
					t.Annotations = p.parseExtendedOption(t.Annotations, ExtendedAnnotation(optname))
				} else {
					p.error("Unsupported Map option: '" + optname + "'")
					return nil
				}
			}
			if p.err != nil {
				return nil
			}
			tok = p.scanner.Scan()
		}
	}
	t.Comment = p.statementEnd(t.Comment)
	return &Type{Variant: TypeVariantMapTypeDef, MapTypeDef: t}
}

func (p *parser) parseNumericType(typeName Identifier, supertypeName TypeRef, comment string) *Type {
	lsupertypeName := strings.ToLower(string(supertypeName))
	switch lsupertypeName {
	case "int32", "int64", "int16", "int8", "float64", "float32":
	default:
		comment = p.statementEnd(comment)
		return makeAliasType(TypeName(typeName), TypeRef(lsupertypeName), comment)
	}
	t := NewNumberTypeDef()
	t.Name = TypeName(typeName)
	t.Type = TypeRef(supertypeName)
	t.Comment = comment
	c := p.skipWhitespaceExceptNewline()
	if c == '(' {
		p.scanner.Next()
		tok := p.scanner.Scan()
		commaExpected := false
		for tok != ')' {
			if commaExpected {
				if tok != ',' {
					p.expectedError("',' or ')'")
					return nil
				}
				tok = p.scanner.Scan()
			} else {
				commaExpected = true
			}
			optname := ""
			if tok != scanner.Ident {
				p.expectedError("option name")
				return nil
			}
			optname = p.scanner.TokenText()
			switch optname {
			case "min":
				p.expect("=")
				if p.err == nil {
					t.Min = newNumber(p.numericLiteral("numeric literal"))
				}
			case "max":
				p.expect("=")
				if p.err == nil {
					t.Max = newNumber(p.numericLiteral("numeric literal"))
				}
			default:
				if strings.HasPrefix(optname, "x_") {
					t.Annotations = p.parseExtendedOption(t.Annotations, ExtendedAnnotation(optname))
				} else {
					p.error("Unsupported Number option: '" + optname + "'")
					return nil
				}
			}
			if p.err != nil {
				return nil
			}
			tok = p.scanner.Scan()
		}
	}
	t.Comment = p.statementEnd(t.Comment)
	return &Type{Variant: TypeVariantNumberTypeDef, NumberTypeDef: t}
}

func (p *parser) parseBoolType(typeName Identifier, supertypeName TypeRef, comment string) *Type {
	t := NewAliasTypeDef()
	t.Name = TypeName(typeName)
	t.Type = TypeRef(supertypeName)
	t.Comment = comment
	c := p.skipWhitespaceExceptNewline()
	if c == '(' {
		p.scanner.Next()
		tok := p.scanner.Scan()
		commaExpected := false
		for tok != ')' {
			if commaExpected {
				if tok != ',' {
					p.expectedError("',' or ')'")
					return nil
				}
				tok = p.scanner.Scan()
			} else {
				commaExpected = true
			}
			optname := ""
			if tok != scanner.Ident {
				p.expectedError("option name")
				return nil
			}
			optname = p.scanner.TokenText()
			if strings.HasPrefix(optname, "x_") {
				t.Annotations = p.parseExtendedOption(t.Annotations, ExtendedAnnotation(optname))
			} else {
				p.error("Unsupported Bool option: '" + optname + "'")
				return nil
			}
			tok = p.scanner.Scan()
		}
	}
	t.Comment = p.statementEnd(t.Comment)
	return &Type{Variant: TypeVariantAliasTypeDef, AliasTypeDef: t}
}

func newNumber(n float64) *Number {
	tr := math.Trunc(n)
	if tr == n {
		i := int64(n)
		return &Number{NumberVariantInt64, nil, nil, nil, &i, nil, nil}
	}
	return &Number{NumberVariantFloat64, nil, nil, nil, nil, nil, &n}
}

func (p *parser) parseUnionType(typeName Identifier, supertypeName TypeRef, comment string) *Type {
	t := NewUnionTypeDef()
	t.Name = TypeName(typeName)
	t.Type = TypeRef(supertypeName)
	t.Comment = comment
	p.expect("<")
	tok := p.scanner.Scan()
	commaExpected := false
	for tok != '>' {
		if commaExpected {
			if tok != ',' {
				p.expectedError("','")
				break
			}
			tok = p.scanner.Scan()
		} else {
			commaExpected = true
		}
		if tok != scanner.Ident {
			p.error("malformed union variant list")
			break
		} else {
			t.Variants = append(t.Variants, TypeRef(p.scanner.TokenText()))
		}
		tok = p.scanner.Scan()
	}
	c := p.skipWhitespaceExceptNewline()
	if c == '(' {
		options := p.parseStructOptions("Union")
		if options != nil {
			if _, ok := options["closed"]; ok {
				p.error("Unsupported Union option: closed")
				return nil
			}
			if len(options) > 0 {
				t.Annotations = options
			}
		}
	}
	t.Comment = p.statementEnd(t.Comment)
	return &Type{Variant: TypeVariantUnionTypeDef, UnionTypeDef: t}
}

func (p *parser) parseEnumType(typeName Identifier, supertypeName TypeRef, comment string) *Type {
	t := NewEnumTypeDef()
	t.Name = TypeName(typeName)
	t.Type = TypeRef(supertypeName)
	t.Comment = comment
	var tok rune
	c := p.skipWhitespaceExceptNewline()
	if c == ';' {
		t.Comment = p.statementEnd(t.Comment)
		return nil
	}
	if c == '(' {
		options := p.parseStructOptions("Enum")
		if options != nil {
			if _, ok := options["closed"]; ok {
				p.error("Unsupported Enum option: closed")
				return nil
			}
			if len(options) > 0 {
				t.Annotations = options
			}
		}
	}
	p.expect("{")
	tok = p.scanner.Scan()
	if tok == scanner.Comment {
		t.Comment, _ = p.parseComment(tok, t.Comment)
		tok = p.scanner.Scan()
	}
	for tok != '}' {
		if tok == scanner.Comment {
			comment, _ = p.parseComment(tok, comment)
		} else if tok != scanner.Ident {
			p.error("Enum type not terminated properly")
			break
		} else {
			symbol := p.scanner.TokenText()
			p.skipWhitespace()
			c := p.scanner.Peek()
			if c == ',' {
				p.scanner.Next()
				p.skipWhitespace()
				c = p.scanner.Peek()
			}
			if c == '/' {
				comment = p.trailingComment(comment)
			}
			el := EnumElementDef{Identifier(symbol), comment}
			t.Elements = append(t.Elements, &el)
			comment = ""
		}
		tok = p.scanner.Scan()
	}
	t.Comment = p.trailingComment(t.Comment)
	return &Type{Variant: TypeVariantEnumTypeDef, EnumTypeDef: t}
}

func (p *parser) registerResource(r *Resource) {
	if p.resources == nil {
		p.resources = make([]*Resource, 0)
	}
	p.resources = append(p.resources, r)
}

func (p *parser) parseResourceOptions() map[ExtendedAnnotation]string {
	options := make(map[ExtendedAnnotation]string)
	c := p.skipWhitespaceExceptNewline()
	if c == '(' {
		p.scanner.Next()
		tok := p.scanner.Scan()
		commaExpected := false
		for tok != ')' {
			if commaExpected {
				if tok != ',' {
					p.expectedError("',' or ')'")
					return nil
				}
				tok = p.scanner.Scan()
			} else {
				commaExpected = true
			}
			optname := ""
			if tok != scanner.Ident {
				p.expectedError("option name")
				return nil
			}
			optname = p.scanner.TokenText()
			switch strings.ToLower(optname) {
			case "async":
				options["async"] = ""
			default:
				if strings.HasPrefix(optname, "x_") {
					options = p.parseExtendedOption(options, ExtendedAnnotation(optname))
				} else {
					p.error("Unsupported resource option: " + optname)
					return nil
				}
			}
			if p.err != nil {
				return nil
			}
			tok = p.scanner.Scan()
		}
	}
	if len(options) == 0 {
		return nil
	}
	return options
}

func (p *parser) parseResource(comment string) *Resource {
	r := NewResource()
	r.Comment = comment
	r.Type = TypeRef(p.identifier("resource type"))
	rt := p.findType(TypeRef(r.Type))
	if rt == nil {
		p.error("Type not found: " + string(r.Type))
		return nil
	}
	method := strings.ToUpper(string(p.identifier("HTTP method")))
	switch method {
	case "GET", "PUT", "DELETE", "POST", "HEAD", "PATCH", "OPTIONS":
		r.Method = method
	default:
		p.error("Bad HTTP method in resource: " + method)
		return nil
	}
	urlTemplate := p.stringLiteral("URL template")
	p.parsePathTemplate(r, urlTemplate)
	c := p.skipWhitespaceExceptNewline()
	if c == '(' {
		options := p.parseResourceOptions()
		if options != nil {
			if _, ok := options["async"]; ok {
				b := true
				r.Async = &b
				delete(options, "async")
			}
		}
		if len(options) > 0 {
			//when the rdl model gets updated to include it
			//r.Annotations = options
		}
	} else if c != '{' {
		p.expectedError("'{'")
		return nil
	}
	fcomment := ""
	tok := p.scanner.Scan()
	for tok != scanner.EOF || p.err == nil {
		if tok == '}' {
			break
		} else {
			switch tok {
			case scanner.Comment:
				fcomment, _ = p.parseComment(tok, fcomment)
			case '#':
				if p.pedantic {
					p.error("legacy line comment character '#' not supported. Use '//'")
					return nil
				}
				if !p.nowarn {
					p.warning("use '//' instead of '#'")
				}
				fcomment = p.parseLegacyComment(fcomment)
			case scanner.Ident:
				sym := p.scanner.TokenText()
				switch sym {
				case "authenticate":
					if r.Auth != nil {
						p.error("Cannot specify more than one authorization permission per resource")
						return nil
					}
					auth := NewResourceAuth()
					auth.Authenticate = true
					r.Auth = auth
					r.Comment = p.statementEnd(r.Comment)
					fcomment = ""
				case "authorize":
					p.parseAuthorization(r)
					fcomment = ""
				case "expected":
					p.parseExpected(r)
					fcomment = ""
				case "exceptions":
					p.parseExceptions(r)
					fcomment = ""
				case "responses":
					p.error("resource 'responses' no longer supported")
					return nil
				case "async":
					b := true
					r.Async = &b
					fcomment = ""
				default:
					c := p.scanner.Peek()
					if c == '.' {
						p.scanner.Next()
						tok := p.scanner.Scan()
						if tok == scanner.Ident {
							sym = sym + "." + p.scanner.TokenText()
						} else {
							p.error("type reference must be a compound name")
							return nil
						}
					}
					p.parseResourceParam(r, sym, fcomment)
					fcomment = ""
				}
			}
		}
		if p.err != nil {
			return nil
		}
		tok = p.scanner.Scan()
	}
	for _, in := range r.Inputs {
		if in.Type == "" {
			p.error("Resource input '" + string(in.Name) + "' has no corresponding type declaration")
			return nil
		}
	}
	if r.Method == "PUT" || r.Method == "POST" {
		ok := false
		for _, in := range r.Inputs {
			if !in.PathParam && in.QueryParam == "" && in.Header == "" && in.Context == "" {
				if ok {
					p.error(r.Method + " on a resource with too many corresponding input parameters")
					return nil
				}
				ok = true
			}
		}
		if !ok {
			p.error(r.Method + " on a resource with no corresponding input parameter")
		}
	}
	return r
}

func (p *parser) parsePathTemplate(r *Resource, template string) {
	var query *string
	var path string
	q := strings.Index(template, "?")
	if q >= 0 {
		s := template[q+1:]
		query = &s
		path = template[:q]
	} else {
		path = template
	}
	r.Path = path
	i := strings.Index(path, "{")
	var inputs []*ResourceInput
	for i >= 0 {
		j := strings.Index(path[i:], "}")
		if j < 0 {
			p.error("bad path template syntax: " + path)
			return
		}
		j += i
		name := path[i+1 : j]
		pattern := ""
		k := strings.Index(name, ":")
		if k >= 0 {
			if k == 0 {
				p.error("Bad path template syntax: " + path)
			}
			s := name[k+1:]
			pattern = s
			name = name[0:k]
		}
		in := NewResourceInput()
		in.Name = Identifier(name)
		in.Pattern = pattern
		in.PathParam = true
		inputs = append(inputs, in)
		i = strings.Index(path[j+1:], "{")
		if i >= 0 {
			i += j + 1
		}
	}
	if query != nil {
		for _, kv := range strings.Split(*query, "&") {
			name := kv
			isFlag := true
			val := "{" + name + "}" //for boolean flag syntax
			i := strings.Index(kv, "=")
			if i == 0 {
				p.error("bad path template syntax: " + r.Path)
				return
			} else if i > 0 {
				val = kv[i+1:]
				name = kv[0:i]
				isFlag = false
			}
			if len(val) < 3 || val[0] != '{' || val[len(val)-1] != '}' {
				p.error("bad path template syntax: " + r.Path)
				return
			}
			field := val[1 : len(val)-1]
			in := NewResourceInput()
			in.Name = Identifier(field)
			in.QueryParam = name
			if isFlag {
				in.Flag = isFlag
			}
			inputs = append(inputs, in)

		}
	}
	if len(inputs) > 0 {
		r.Inputs = inputs
	}
}

func (p *parser) parseResourceParam(r *Resource, paramTypeName string, comment string) {
	//factor this, it's too big

	paramType := p.findType(TypeRef(paramTypeName))
	if paramType == nil {
		p.error("Undefined type: " + paramTypeName)
		return
	}

	tok := p.scanner.Scan()
	if tok == '<' {
		if paramTypeName != "array" {
			p.expectedError("String array")
			return
		}
		items := TypeRef(p.identifier("type name"))
		p.expect(">")
		if p.err == nil {
			if !p.registry.IsStringTypeName(items) {
				p.expectedError("String array")
				return
			}
			p.error("array parameters NYI")
			return
		}
		tok = p.scanner.Scan()
	}

	if tok != scanner.Ident {
		p.expectedError("param name")
	}
	paramName := p.scanner.TokenText()
	p.skipWhitespaceExceptNewline()

	pathOrQueryParam := false
	var input *ResourceInput
	current := -1
	for i, in := range r.Inputs {
		if string(in.Name) == paramName {
			current = i
			input = in
			pathOrQueryParam = true
			break
		}
	}
	if !pathOrQueryParam {
		input = NewResourceInput()
		input.Name = Identifier(paramName)
	}
	input.Comment = comment
	input.Type = TypeRef(paramTypeName)
	output := false
	if p.scanner.Peek() == '(' {
		p.scanner.Next()
		output = p.parseResourceParamOptions(tok, pathOrQueryParam, input)
		if p.err != nil {
			return
		}
	}
	input.Comment = p.statementEnd(input.Comment)
	if output {
		p.addOutput(r, paramName, input)
	} else {
		input.Name = Identifier(paramName)
		if current < 0 {
			r.Inputs = append(r.Inputs, input)
		} else {
			r.Inputs[current] = input
		}
	}
}

func (p *parser) addOutput(r *Resource, paramName string, input *ResourceInput) {
	out := NewResourceOutput()
	out.Name = Identifier(paramName)
	out.Type = input.Type
	out.Header = input.Header
	out.Optional = input.Optional
	r.Outputs = append(r.Outputs, out)
}

func (p *parser) parseDefaultValue(typeName TypeRef) interface{} {
	var val interface{}
	p.expect("=")
	bt := p.baseTypeByName(typeName)
	switch bt {
	case BaseTypeString:
		s := p.stringLiteral("String literal")
		val = s
	case BaseTypeInt8, BaseTypeInt16, BaseTypeInt32, BaseTypeInt64, BaseTypeFloat32, BaseTypeFloat64:
		s := p.numericLiteral(fmt.Sprintf("%v literal", bt))
		val = s
	case BaseTypeBool:
		s := p.identifier("'true' or 'false'")
		b := "true" == s
		val = b
	case BaseTypeEnum:
		s := p.identifier("enum symbol")
		val = s
	default:
		p.error(fmt.Sprintf("cannot provide default value for a %v type", bt))
		val = nil
	}
	return val
}

func (p *parser) parseResourceParamOptions(tok rune, pathOrQueryParam bool, input *ResourceInput) bool {
	optional := false
	output := false

	for tok != ')' && tok != scanner.EOF && p.err == nil {
		option := p.identifier("parameter option")
		if p.err == nil {
			switch option {
			case "default":
				v := p.parseDefaultValue(input.Type)
				if v != nil {
					input.Default = v
				}
			case "required":
				if !p.acceptLegacy("required", "omit 'required', it is the default") {
					return false
				}
				optional = false
			case "optional":
				optional = true
			case "out":
				if pathOrQueryParam {
					p.error("Cannot make a path or queryparam an output")
					return false
				}
				output = true
			case "header":
				if pathOrQueryParam {
					p.error("Cannot make a path or queryparam a header param")
					return false
				}
				p.expect("=")
				s := p.stringLiteral("header name")
				input.Header = s
			case "context":
				if !p.nowarn {
					p.warning("Deprecated resource param option: 'context=...'.")
				}
				p.expect("=")
				s := p.stringLiteral("quoted context variable name")
				input.Context = s
			}
			if p.err != nil {
				return false
			}
		}
		tok = p.scanner.Scan()
	}
	input.Optional = optional
	return output
}

func (p *parser) isEndOfStatement(comment string) (bool, string) {
	c := p.scanner.Peek()
	if c == ';' {
		p.scanner.Next()
		return true, comment
	}
	for c != '\n' && p.isWhitespace(c) {
		p.scanner.Next()
		c = p.scanner.Peek()
	}
	if c == '/' {
		return true, p.trailingComment(comment)
	}
	return false, comment
}

func (p *parser) parseExpected(r *Resource) {
	//sym; or sym, sym;
	sym := p.identifier("symbol")
	if sym == "" {
		return
	}
	r.Expected = string(sym)
	var alternatives []string
	c := p.skipWhitespaceExceptNewline()
	for c == ',' {
		p.scanner.Next()
		tmp := p.identifier("symbol")
		alternatives = append(alternatives, string(tmp))
		c = p.skipWhitespaceExceptNewline()
	}
	if len(alternatives) > 0 {
		r.Alternatives = alternatives
	}
	r.Comment = p.statementEnd(r.Comment)
}

func (p *parser) parseExceptions(r *Resource) {
	if !p.expect("{") {
		return
	}
	exceptions := make(map[string]*ExceptionDef)
	tok := p.scanner.Scan()
	for tok != scanner.EOF {
		if tok == '}' {
			break
		}
		switch tok {
		case scanner.Comment:
			cmt, _ := p.parseComment(tok, r.Comment)
			r.Comment = cmt
		case scanner.Ident:
			etype := p.scanner.TokenText()
			ft := p.findType(TypeRef(etype))
			if ft == nil {
				if etype != "ResourceError" { //we generate this
					p.error("No such type: " + etype)
				}
			}
			esym := p.identifier("symbol")
			if esym == "" {
				return
			}
			edef := NewExceptionDef()
			edef.Type = etype
			edef.Comment = p.statementEnd("")
			exceptions[string(esym)] = edef
		}
		tok = p.scanner.Scan()
	}
	if len(exceptions) > 0 {
		r.Exceptions = exceptions
	}
}

func (p *parser) parseAuthorization(r *Resource) {
	if r.Auth != nil {
		p.error("Cannot specify more than one authorization permission per resource")
	} else {
		tok := p.scanner.Scan()
		if tok != '(' {
			p.expectedError("(")
		} else {
			auth := NewResourceAuth()
			tok := p.scanner.Scan()
			for tok != ')' && tok != scanner.EOF {
				if tok != ',' {
					s := p.scanner.TokenText()
					q, err := strconv.Unquote(s)
					if err != nil {
						p.error("Improperly escaped string: " + s)
					}
					if auth.Action == "" {
						auth.Action = q
					} else if auth.Resource == "" {
						auth.Resource = q
					} else if auth.Domain == "" {
						auth.Domain = q
					} else {
						p.error("too many options for the authorize statement")
						return
					}
				}
				tok = p.scanner.Scan()
			}
			r.Auth = auth
			r.Comment = p.statementEnd(r.Comment)
		}
	}
}

func (p *parser) acceptLegacy(item string, warning string) bool {
	if p.pedantic {
		p.error("legacy feature not supported: " + item)
		return false
	}
	if !p.nowarn {
		p.warning(warning)
	}
	return true
}
