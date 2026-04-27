// Package parse implements the mmk DSL parser.
//
// Grammar:
//
//	file       := (directive | newline | comment)*
//	directive  := deftype | defrunner | target_rule
//	deftype    := 'deftype' name body
//	defrunner  := 'defrunner' name ('setup' | 'cleanup')? body
//	target_rule:= type? (target|pattern) ('on' runner)? (':' dep*)? body?
//	body       := '{' ... '}' (balanced braces, arbitrary content)
//	name       := word | string
//	dep        := word | string
//	target     := word | string   (concrete name)
//	pattern    := '...'           (single-quoted regex; anchored ^ and $)
//
// Words are non-whitespace sequences not containing ':', '{', '}', '"', '\'', '#'.
// Strings are double-quoted, supporting \" escapes.
// Patterns are single-quoted; content is the raw regex, no escape processing.
// Comments begin with '#' and extend to end of line.
// Body braces may be on the same line as the header or a subsequent line.
// In a target_rule, exactly one of target or pattern is set.
// Only the target slot may be a pattern; type, runner, and deps are always names.
package parse

import (
	"fmt"
	"slices"
	"strings"
)

type File struct {
	Directives []Directive
}

type Directive interface {
	isDirective()
}

// DefType defines how to compute the artifact date for targets of the given type.
// Body is raw bash that prints the artifact's timestamp to stdout:
// epoch seconds (all digits) or RFC3339/RFC3339Nano. Non-zero exit means the
// artifact doesn't exist. $target and $deps are available.
type DefType struct {
	Name string
	Body string
}

// DefRunner defines one phase of a runner type's execution.
// Phase is "", "setup", or "cleanup". Empty phase defines the mandatory run
// body; "setup" runs once before any targets use this runner; "cleanup" runs
// once after the mmk execution finishes. $target, $deps, and $MMK_GENFILE are
// available in all phases; the run phase additionally receives $MMK_RUNNER_STATE,
// $MMK_FUNC, $MMK_TARGET, and $MMK_DEPS.
type DefRunner struct {
	Name  string
	Phase string // "", "setup", or "cleanup"
	Body  string
}

// Dep is a single dependency in a target rule.
// Verb is non-empty when the dep is a verb-qualified reference like [clean somedep].
type Dep struct {
	Target string
	Verb   string
}

// Option is a key=value annotation attached to a target rule's header.
// Options are exported as bash variables to bodies of the rule they're
// declared on. Runners and bodies decide what (if anything) to honor.
type Option struct {
	Key   string
	Value string
}

// TargetRule is a single build target.
// Exactly one of Target or Pattern is set.
// Verb is non-empty for verb rules declared with [verb target] syntax.
type TargetRule struct {
	Type    string // empty if no type
	Target  string // concrete name; empty if Pattern is set
	Pattern string // regex from '...'; empty if Target is set
	Runner  string // empty if no runner
	Verb        string   // empty for default build rules
	HasDepSep   bool     // true if ':' was present (even with empty dep list)
	AugmentDeps bool     // true if ':+' was used: deps inherit AND extend the default rule's deps (verb rules only)
	Deps        []Dep    // may be nil
	Options     []Option // key=value annotations from the rule header; preserves source order
	Body        string   // empty if no body (deps-only rule)
}

// DefBody defines the default Run body for targets of the given type.
// When a typed target has no explicit body, this body is used instead of a no-op.
// Verb is non-empty for verb-specific default bodies (defbody type verb { ... }).
// $target and $deps are available, same as in any target body.
// Options are key=value annotations on the defbody header — the same shape
// as TargetRule.Options. The defbody's options apply to every verb-rule
// node that uses this defbody (when no per-rule body is set).
type DefBody struct {
	Type    string
	Verb    string
	Body    string
	Options []Option
}

// Passthrough is a raw line of bash that is not an mmk directive.
// It is passed through verbatim to the generated script.
type Passthrough struct {
	Line string
}

func (*DefType) isDirective()     {}
func (*DefRunner) isDirective()   {}
func (*DefBody) isDirective()     {}
func (*TargetRule) isDirective()  {}
func (*Passthrough) isDirective() {}

// Parse parses src and returns the AST or an error.
func Parse(src []byte) (*File, error) {
	p := &parser{s: &scanner{src: src, line: 1}}
	return p.parseFile()
}

// --- scanner ---

type scanner struct {
	src  []byte
	pos  int
	line int
}

func (s *scanner) peek() byte {
	if s.pos >= len(s.src) {
		return 0
	}
	return s.src[s.pos]
}

func (s *scanner) advance() byte {
	if s.pos >= len(s.src) {
		return 0
	}
	b := s.src[s.pos]
	s.pos++
	if b == '\n' {
		s.line++
	}
	return b
}

func (s *scanner) skipHorizontalSpace() {
	for b := s.peek(); b == ' ' || b == '\t'; b = s.peek() {
		s.advance()
	}
}

func (s *scanner) skipToEndOfLine() {
	for b := s.peek(); b != '\n' && b != 0; b = s.peek() {
		s.advance()
	}
}

func isWordByte(b byte) bool {
	switch b {
	case 0, ' ', '\t', '\n', ':', '{', '}', '#', '"', '\'', '(', ')', '[', ']':
		return false
	}
	return true
}

// colonIsSeparator reports whether the ':' at the current scanner position
// is the dep-list separator (followed by space, tab, newline, '{', '#', '+',
// or EOF) rather than an embedded ':' within a name like "image:tag". The '+'
// case covers the ':+' augment-deps separator on verb rules.
func (s *scanner) colonIsSeparator() bool {
	next := s.pos + 1
	if next >= len(s.src) {
		return true
	}
	switch s.src[next] {
	case ' ', '\t', '\n', '{', '#', '+', 0:
		return true
	}
	return false
}

func (s *scanner) readWord() string {
	var sb strings.Builder
	for {
		b := s.peek()
		if isWordByte(b) {
			sb.WriteByte(s.advance())
		} else if b == ':' && !s.colonIsSeparator() {
			sb.WriteByte(s.advance()) // ':' is part of the name, e.g. "image:tag"
		} else {
			break
		}
	}
	return sb.String()
}

func (s *scanner) readString() (string, error) {
	line := s.line
	s.advance() // consume opening "
	var sb strings.Builder
	for {
		b := s.peek()
		if b == 0 {
			return "", fmt.Errorf("line %d: unterminated string", line)
		}
		if b == '\\' {
			s.advance()
			sb.WriteByte('\\')
			sb.WriteByte(s.advance())
			continue
		}
		if b == '"' {
			s.advance()
			return sb.String(), nil
		}
		sb.WriteByte(s.advance())
	}
}

// readSingleQuoted reads a single-quoted pattern string '...' and returns
// the raw content between the quotes. No escape processing is performed.
func (s *scanner) readSingleQuoted() (string, error) {
	line := s.line
	s.advance() // consume opening '
	var sb strings.Builder
	for {
		b := s.peek()
		if b == 0 {
			return "", fmt.Errorf("line %d: unterminated pattern string", line)
		}
		if b == '\'' {
			s.advance() // consume closing '
			return sb.String(), nil
		}
		sb.WriteByte(s.advance())
	}
}

// readBody reads raw content between balanced braces.
// The opening '{' must already be consumed. openedAt is the line of that '{',
// used for error messages.
//
// Braces inside double-quoted strings, single-quoted strings, and # comments
// are not counted toward depth, so bodies containing bash like echo "{" or
// function definitions work correctly.
func (s *scanner) readBody(openedAt int) (string, error) {
	var sb strings.Builder
	depth := 1
	for depth > 0 {
		b := s.peek()
		switch b {
		case 0:
			return "", fmt.Errorf("line %d: no closing '}' for body opened at line %d", s.line, openedAt)
		case '{':
			depth++
			sb.WriteByte(s.advance())
		case '}':
			depth--
			if depth == 0 {
				s.advance() // consume closing }, don't include in body
			} else {
				sb.WriteByte(s.advance())
			}
		case '"':
			if err := s.readQuotedInto(&sb, '"', true); err != nil {
				return "", err
			}
		case '\'':
			if err := s.readQuotedInto(&sb, '\'', false); err != nil {
				return "", err
			}
		case '`':
			if err := s.readQuotedInto(&sb, '`', true); err != nil {
				return "", err
			}
		case '#':
			// Bash comment: copy through to EOL without counting braces.
			for s.peek() != '\n' && s.peek() != 0 {
				sb.WriteByte(s.advance())
			}
		default:
			sb.WriteByte(s.advance())
		}
	}
	return sb.String(), nil
}

// readQuotedInto copies a quoted string (including its delimiters) into sb.
// If allowBackslash, backslash-escape sequences are passed through as-is.
// Single-quoted bash strings do not honour backslash.
func (s *scanner) readQuotedInto(sb *strings.Builder, quote byte, allowBackslash bool) error {
	openedAt := s.line
	sb.WriteByte(s.advance()) // opening quote
	for {
		b := s.peek()
		if b == 0 {
			return fmt.Errorf("line %d: unterminated quoted string opened at line %d", s.line, openedAt)
		}
		if allowBackslash && b == '\\' {
			sb.WriteByte(s.advance()) // backslash
			sb.WriteByte(s.advance()) // next char (even if it's the quote)
			continue
		}
		if b == quote {
			sb.WriteByte(s.advance()) // closing quote
			return nil
		}
		sb.WriteByte(s.advance())
	}
}

// --- parser ---

type parser struct {
	s *scanner
}

func (p *parser) skipWhitespaceAndComments() {
	for {
		p.s.skipHorizontalSpace()
		switch p.s.peek() {
		case '\n':
			p.s.advance()
		case '#':
			p.s.skipToEndOfLine()
		default:
			return
		}
	}
}

// parseName reads a word or double-quoted string.
// Single-quoted patterns are not valid here; use parseHeaderToken for header positions.
func (p *parser) parseName() (string, error) {
	p.s.skipHorizontalSpace()
	switch p.s.peek() {
	case '"':
		return p.s.readString()
	case '\'':
		return "", fmt.Errorf("line %d: single-quoted patterns are only valid as target names", p.s.line)
	default:
		b := p.s.peek()
		if !isWordByte(b) && !(b == ':' && !p.s.colonIsSeparator()) {
			return "", fmt.Errorf("line %d: expected name, got %q", p.s.line, p.s.peek())
		}
		return p.s.readWord(), nil
	}
}

// headerToken is a parsed token from a target rule header.
type headerToken struct {
	val       string
	isPattern bool   // true if read from a single-quoted '...' pattern
	verb      string // non-empty when read from a [verb target] bracketed pair
}

// parseBracketed reads a '[verb target]' pair and returns (verb, target, isPattern, err).
// The opening '[' must be the current peek byte.
func (p *parser) parseBracketed() (verb, target string, isPattern bool, err error) {
	openedAt := p.s.line
	p.s.advance() // consume '['
	p.s.skipHorizontalSpace()
	verb = p.s.readWord()
	if verb == "" {
		err = fmt.Errorf("line %d: expected verb inside '[...]'", openedAt)
		return
	}
	p.s.skipHorizontalSpace()
	var tok headerToken
	tok, err = p.parseHeaderToken()
	if err != nil {
		return
	}
	if tok.verb != "" {
		err = fmt.Errorf("line %d: nested bracketed verb not allowed", p.s.line)
		return
	}
	target = tok.val
	isPattern = tok.isPattern
	p.s.skipHorizontalSpace()
	if p.s.peek() != ']' {
		err = fmt.Errorf("line %d: expected ']' after verb target, got %q", p.s.line, p.s.peek())
		return
	}
	p.s.advance() // consume ']'
	return
}

// parseHeaderToken reads a word, double-quoted string, single-quoted pattern, or [verb target].
func (p *parser) parseHeaderToken() (headerToken, error) {
	p.s.skipHorizontalSpace()
	switch p.s.peek() {
	case '\'':
		val, err := p.s.readSingleQuoted()
		if err != nil {
			return headerToken{}, err
		}
		return headerToken{val: val, isPattern: true}, nil
	case '"':
		val, err := p.s.readString()
		if err != nil {
			return headerToken{}, err
		}
		return headerToken{val: val}, nil
	case '[':
		verb, target, isPattern, err := p.parseBracketed()
		if err != nil {
			return headerToken{}, err
		}
		return headerToken{val: target, isPattern: isPattern, verb: verb}, nil
	default:
		b := p.s.peek()
		if !isWordByte(b) && !(b == ':' && !p.s.colonIsSeparator()) {
			return headerToken{}, fmt.Errorf("line %d: expected name, got %q", p.s.line, p.s.peek())
		}
		word := p.s.readWord()
		// If the word looks like IDENT="..." (option key, equals, then a
		// double-quoted value), consume the quoted value and concat. This lets
		// option values contain spaces and other shell-meaningful chars.
		if strings.HasSuffix(word, "=") && p.s.peek() == '"' && isOptionKeyPrefix(word[:len(word)-1]) {
			val, err := p.s.readString()
			if err != nil {
				return headerToken{}, err
			}
			word += val
		}
		return headerToken{val: word}, nil
	}
}

func (p *parser) parseFile() (*File, error) {
	var f File
	for {
		p.skipWhitespaceAndComments()
		if p.s.peek() == 0 {
			break
		}
		d, err := p.parseDirectiveOrPassthrough()
		if err != nil {
			return nil, err
		}
		f.Directives = append(f.Directives, d)
	}
	return &f, nil
}

// parseDirectiveOrPassthrough applies the "commit and error" heuristic:
// if the line looks like an mmk directive (starts with deftype/defrunner, or
// contains '{' or ':' before newline), parse as directive (errors are clear).
// Otherwise read the rest of the line as a Passthrough.
func (p *parser) parseDirectiveOrPassthrough() (Directive, error) {
	// Peek at first word without consuming.
	saved, savedLine := p.s.pos, p.s.line
	word := p.s.readWord()
	p.s.pos, p.s.line = saved, savedLine

	if word == "deftype" || word == "defrunner" || word == "defbody" {
		return p.parseDirective()
	}

	// A bash function definition (first word immediately followed by '(') is
	// always passthrough. The subsequent body lines are each their own
	// passthrough, so no special multi-line handling is needed here.
	if p.firstWordFollowedByParen() {
		line := p.readRestOfLine()
		return &Passthrough{Line: line}, nil
	}

	// A bash variable assignment (IDENT=...) is always passthrough. Without
	// this, lines like `FOO=value:tag` get parsed as target rules because of
	// the embedded ':'.
	if isVarAssignmentPrefix(word) {
		line := p.readRestOfLine()
		return &Passthrough{Line: line}, nil
	}

	// Scan the rest of the line for ':' or '{' to decide whether to commit.
	if p.lineHasDirectiveMarker() {
		return p.parseDirective()
	}

	// No marker found — treat the whole line as passthrough bash.
	line := p.readRestOfLine()
	return &Passthrough{Line: line}, nil
}

// isVarAssignmentPrefix reports whether word starts with `IDENT=`, where IDENT
// is a valid bash variable name. Used to recognise lines that are bash variable
// assignments and treat them as passthrough rather than target rules.
func isVarAssignmentPrefix(word string) bool {
	eq := strings.IndexByte(word, '=')
	if eq <= 0 {
		return false
	}
	for i := 0; i < eq; i++ {
		b := word[i]
		switch {
		case b >= 'A' && b <= 'Z':
		case b >= 'a' && b <= 'z':
		case b == '_':
		case b >= '0' && b <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// firstWordFollowedByParen returns true if the first word on the current line
// is immediately followed (possibly after spaces) by '('. This identifies bash
// function definitions, which must be treated as passthrough.
func (p *parser) firstWordFollowedByParen() bool {
	pos, line := p.s.pos, p.s.line
	defer func() { p.s.pos, p.s.line = pos, line }()
	p.s.readWord()
	p.s.skipHorizontalSpace()
	return p.s.peek() == '('
}

// lineHasDirectiveMarker scans from the current position and returns true if
// the content should be parsed as an mmk directive rather than passed through.
// Commits if: first token is a quote (quoted target or pattern), ':' or '{'
// appears on the current line, or '{' appears at the start of the next
// non-blank line (body-on-next-line syntax).
func (p *parser) lineHasDirectiveMarker() bool {
	pos := p.s.pos
	line := p.s.line
	defer func() { p.s.pos, p.s.line = pos, line }()

	// A leading quote is unambiguously a directive (quoted target or pattern).
	if b := p.s.peek(); b == '\'' || b == '"' {
		return true
	}

	for {
		b := p.s.peek()
		switch b {
		case 0, '\n':
			// End of line without finding a marker; look ahead for body on next line.
			return p.nextNonBlankIs('{')
		case ':', '{':
			return true
		case '#':
			// Comment runs to EOL; look ahead for body on next line.
			return p.nextNonBlankIs('{')
		case '"':
			// Skip double-quoted string. Unterminated means the string spans
			// lines — that's bash, not an mmk directive, so treat as passthrough.
			p.s.advance()
			for {
				c := p.s.peek()
				if c == 0 || c == '\n' {
					return false
				}
				if c == '\\' {
					p.s.advance()
					p.s.advance()
					continue
				}
				p.s.advance()
				if c == '"' {
					break
				}
			}
		case '\'':
			// Skip single-quoted string. Unterminated means the string spans
			// lines — that's bash, not an mmk pattern, so treat as passthrough.
			p.s.advance()
			for {
				c := p.s.peek()
				if c == 0 || c == '\n' {
					return false
				}
				p.s.advance()
				if c == '\'' {
					break
				}
			}
		default:
			p.s.advance()
		}
	}
}

// nextNonBlankIs skips whitespace and '#' comments and returns true if the
// next meaningful byte equals want. Used by lineHasDirectiveMarker within its
// position-restoring defer, so consumed bytes are automatically restored.
func (p *parser) nextNonBlankIs(want byte) bool {
	for {
		b := p.s.peek()
		if b == ' ' || b == '\t' || b == '\n' {
			p.s.advance()
			continue
		}
		if b == '#' {
			for p.s.peek() != '\n' && p.s.peek() != 0 {
				p.s.advance()
			}
			continue
		}
		return b == want
	}
}

// readRestOfLine reads all bytes from the current position to the end of the
// line (exclusive of the newline) and advances past the newline.
func (p *parser) readRestOfLine() string {
	var sb strings.Builder
	for {
		b := p.s.peek()
		if b == 0 || b == '\n' {
			if b == '\n' {
				p.s.advance()
			}
			return sb.String()
		}
		sb.WriteByte(p.s.advance())
	}
}

func (p *parser) parseDirective() (Directive, error) {
	// Peek at the first word to determine directive type.
	saved, savedLine := p.s.pos, p.s.line
	word := p.s.readWord()
	p.s.pos, p.s.line = saved, savedLine

	switch word {
	case "deftype":
		return p.parseDefBlock(word, func(name, body string) Directive {
			return &DefType{Name: name, Body: body}
		})
	case "defrunner":
		return p.parseDefRunner()
	case "defbody":
		return p.parseDefBody()
	default:
		return p.parseTargetRule()
	}
}

func (p *parser) parseDefBlock(keyword string, make func(name, body string) Directive) (Directive, error) {
	p.s.readWord() // consume keyword
	name, err := p.parseName()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", keyword, err)
	}
	p.skipWhitespaceAndComments()
	if p.s.peek() != '{' {
		return nil, fmt.Errorf("line %d: expected '{' after %s %s", p.s.line, keyword, name)
	}
	openedAt := p.s.line
	p.s.advance() // consume {
	body, err := p.s.readBody(openedAt)
	if err != nil {
		return nil, err
	}
	return make(name, body), nil
}

// parseDefBody handles 'defbody type [verb]? [key=value...]? { body }'.
// Optional tokens after the type name are interpreted as the verb (the first
// bare word) and as key=value options (one or more); they may appear in any
// order between the type and the opening brace.
func (p *parser) parseDefBody() (Directive, error) {
	p.s.readWord() // consume "defbody"
	typeName, err := p.parseName()
	if err != nil {
		return nil, fmt.Errorf("defbody: %w", err)
	}
	var verb string
	var options []Option
	for {
		p.s.skipHorizontalSpace()
		b := p.s.peek()
		if b == 0 || b == '\n' || b == '{' || b == '#' {
			break
		}
		if !isWordByte(b) && b != '"' {
			return nil, fmt.Errorf("line %d: unexpected %q after defbody %s", p.s.line, b, typeName)
		}
		// readWord-equivalent (preserve quoted strings via parseName).
		var word string
		if b == '"' {
			word, err = p.s.readString()
			if err != nil {
				return nil, err
			}
		} else {
			word = p.s.readWord()
		}
		if k, v, ok := parseOptionToken(word); ok {
			if k == "target" || k == "deps" || strings.HasPrefix(k, "MMK_") {
				return nil, fmt.Errorf("line %d: option key %q is reserved", p.s.line, k)
			}
			options = append(options, Option{Key: k, Value: v})
			continue
		}
		if verb != "" {
			return nil, fmt.Errorf("line %d: unexpected token %q after defbody %s %s", p.s.line, word, typeName, verb)
		}
		verb = word
	}
	p.skipWhitespaceAndComments()
	if p.s.peek() != '{' {
		return nil, fmt.Errorf("line %d: expected '{' after defbody %s", p.s.line, typeName)
	}
	openedAt := p.s.line
	p.s.advance()
	body, err := p.s.readBody(openedAt)
	if err != nil {
		return nil, err
	}
	return &DefBody{Type: typeName, Verb: verb, Body: body, Options: options}, nil
}

// parseDefRunner handles 'defrunner name [setup|cleanup]? { body }' directives.
// An optional phase word ("setup" or "cleanup") may appear on the same line as
// the name, before the opening brace. Omitting the phase defines the run body.
func (p *parser) parseDefRunner() (Directive, error) {
	p.s.readWord() // consume "defrunner"
	name, err := p.parseName()
	if err != nil {
		return nil, fmt.Errorf("defrunner: %w", err)
	}
	p.s.skipHorizontalSpace()
	var phase string
	if b := p.s.peek(); b != 0 && b != '\n' && b != '{' && b != '#' {
		if isWordByte(b) || b == '"' {
			phase, err = p.parseName()
			if err != nil {
				return nil, fmt.Errorf("defrunner phase: %w", err)
			}
		}
	}
	if phase != "" && phase != "setup" && phase != "cleanup" {
		return nil, fmt.Errorf("line %d: defrunner %s: unknown phase %q (want \"setup\" or \"cleanup\")", p.s.line, name, phase)
	}
	p.skipWhitespaceAndComments()
	if p.s.peek() != '{' {
		return nil, fmt.Errorf("line %d: expected '{' after defrunner %s", p.s.line, name)
	}
	openedAt := p.s.line
	p.s.advance()
	body, err := p.s.readBody(openedAt)
	if err != nil {
		return nil, err
	}
	return &DefRunner{Name: name, Phase: phase, Body: body}, nil
}

func (p *parser) parseTargetRule() (*TargetRule, error) {
	// Collect header tokens (words/strings/patterns) until the dep-separator ':',
	// '{', '#', newline, or EOF. An embedded ':' within a name (e.g. "image:tag")
	// is not a separator — colonIsSeparator disambiguates the two cases.
	var header []headerToken
	for {
		p.s.skipHorizontalSpace()
		b := p.s.peek()
		if b == '\n' || b == 0 || b == '{' || b == '#' {
			break
		}
		if b == ':' && p.s.colonIsSeparator() {
			break
		}
		tok, err := p.parseHeaderToken()
		if err != nil {
			return nil, err
		}
		header = append(header, tok)
	}

	// Pull off any IDENT=value tokens as options before interpreting the rest
	// as type/target/runner. Options can appear anywhere in the header.
	header, options, err := splitOptions(header)
	if err != nil {
		return nil, fmt.Errorf("line %d: %w", p.s.line, err)
	}

	typ, target, pattern, runner, verb, err := parseHeader(header)
	if err != nil {
		return nil, fmt.Errorf("line %d: %w", p.s.line, err)
	}
	rule := &TargetRule{Type: typ, Target: target, Pattern: pattern, Runner: runner, Verb: verb, Options: options}

	// Optional deps after ':' (or ':+' for augment-inherit mode on verb rules).
	p.s.skipHorizontalSpace()
	if p.s.peek() == ':' {
		p.s.advance()
		rule.HasDepSep = true
		if p.s.peek() == '+' {
			if rule.Verb == "" {
				return nil, fmt.Errorf("line %d: ':+' is only valid on verb rules", p.s.line)
			}
			p.s.advance()
			rule.AugmentDeps = true
		}
		for {
			p.s.skipHorizontalSpace()
			b := p.s.peek()
			if b == '\n' || b == 0 || b == '{' || b == '#' {
				break
			}
			if b == '[' {
				dverb, dtarget, _, err := p.parseBracketed()
				if err != nil {
					return nil, err
				}
				rule.Deps = append(rule.Deps, Dep{Target: dtarget, Verb: dverb})
			} else {
				name, err := p.parseName()
				if err != nil {
					return nil, err
				}
				rule.Deps = append(rule.Deps, Dep{Target: name})
			}
		}
	}

	// Skip inline comment if present.
	p.s.skipHorizontalSpace()
	if p.s.peek() == '#' {
		p.s.skipToEndOfLine()
	}

	// Optional body — '{' may be on the same line or a subsequent line.
	p.skipWhitespaceAndComments()
	if p.s.peek() == '{' {
		openedAt := p.s.line
		p.s.advance()
		body, err := p.s.readBody(openedAt)
		if err != nil {
			return nil, err
		}
		rule.Body = body
	}

	return rule, nil
}

// splitOptions walks the header tokens and pulls out any that look like
// `IDENT=value`, returning the remaining tokens and the parsed options in
// source order. Pattern and verb-bracketed tokens are never options.
//
// Keys named "target" or "deps", or starting with "MMK_", are reserved (they
// would shadow mmk's own bash variables) and produce a parse error.
func splitOptions(tokens []headerToken) ([]headerToken, []Option, error) {
	var rest []headerToken
	var opts []Option
	for _, tok := range tokens {
		if tok.isPattern || tok.verb != "" {
			rest = append(rest, tok)
			continue
		}
		key, val, ok := parseOptionToken(tok.val)
		if !ok {
			rest = append(rest, tok)
			continue
		}
		if key == "target" || key == "deps" || strings.HasPrefix(key, "MMK_") {
			return nil, nil, fmt.Errorf("option key %q is reserved", key)
		}
		opts = append(opts, Option{Key: key, Value: val})
	}
	return rest, opts, nil
}

// parseOptionToken splits a header word into key and value if it has the
// shape `IDENT=value`. Returns ok=false otherwise.
func parseOptionToken(word string) (key, val string, ok bool) {
	eq := strings.IndexByte(word, '=')
	if eq <= 0 {
		return "", "", false
	}
	if !isOptionKeyPrefix(word[:eq]) {
		return "", "", false
	}
	return word[:eq], word[eq+1:], true
}

// isOptionKeyPrefix reports whether s is a valid bash variable name (used as
// an option key).
func isOptionKeyPrefix(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		b := s[i]
		switch {
		case b >= 'A' && b <= 'Z':
		case b >= 'a' && b <= 'z':
		case b == '_':
		case b >= '0' && b <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// parseHeader interprets the header token slice as: type? (target|pattern) ('on' runner)?
// or a single [verb target] bracketed token for verb rules.
// Exactly one of target or pattern will be non-empty in the return values.
func parseHeader(tokens []headerToken) (typ, target, pattern, runner, verb string, err error) {
	if len(tokens) == 0 {
		return "", "", "", "", "", fmt.Errorf("expected target name")
	}

	// A single bracketed token is a verb rule: [verb target]
	if len(tokens) == 1 && tokens[0].verb != "" {
		if tokens[0].isPattern {
			pattern = tokens[0].val
		} else {
			target = tokens[0].val
		}
		verb = tokens[0].verb
		return
	}

	// Split on the unquoted keyword "on".
	onIdx := slices.IndexFunc(tokens, func(t headerToken) bool {
		return t.val == "on" && !t.isPattern && t.verb == ""
	})
	nameTokens := tokens
	if onIdx >= 0 {
		nameTokens = tokens[:onIdx]
		rest := tokens[onIdx+1:]
		if len(rest) != 1 {
			return "", "", "", "", "", fmt.Errorf("expected exactly one runner name after 'on'")
		}
		if rest[0].isPattern {
			return "", "", "", "", "", fmt.Errorf("runner name cannot be a pattern")
		}
		runner = rest[0].val
	}

	setTarget := func(tok headerToken) {
		if tok.isPattern {
			pattern = tok.val
		} else {
			target = tok.val
		}
	}

	switch len(nameTokens) {
	case 1:
		verb = nameTokens[0].verb // non-empty when token is a [verb target] bracket
		setTarget(nameTokens[0])
	case 2:
		if nameTokens[0].isPattern {
			err = fmt.Errorf("type cannot be a pattern")
			return
		}
		typ = nameTokens[0].val
		setTarget(nameTokens[1])
	case 0:
		err = fmt.Errorf("expected target name before 'on'")
	default:
		err = fmt.Errorf("unexpected tokens in header: %v", nameTokens)
	}
	return
}
