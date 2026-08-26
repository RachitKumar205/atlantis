package dsl

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Lexer turns a source byte stream into a sequence of Tokens.
//
// Hand-written rather than table-driven: the grammar holds well under 100
// keywords, and a hand-written scanner carries exact positions into its error
// messages with no runtime dependency.
//
// The lexer does NOT consume comments — they are discarded before the parser
// ever sees them. Whitespace is likewise discarded.
//
// Errors are surfaced via TokError tokens carrying the message in Value, so
// the parser can keep advancing and surface multiple errors per file rather
// than bailing at the first one.
type Lexer struct {
	src  []byte
	file string

	pos     int // byte offset of the next rune to scan
	line    int // 1-indexed
	col     int // 1-indexed, in runes
	prevCol int // saved column to restore on newline (only matters for tests)

	// armedForRawSQL is set after a `touches` keyword is emitted. The
	// next `{` the lexer produces enters raw-capture mode, where the body
	// is consumed verbatim until the matching `}`. Without this, SQL-
	// specific characters inside the body (single quotes, `--` line
	// comments, the `*` in `SELECT *`) would crash the regular lexer.
	armedForRawSQL bool

	// sawPartial is set when TokPartial is emitted and cleared at the
	// following TokWhere (or the entity-closing `}`); it gates armedForWhere
	// so only a partial-index `where` triggers raw predicate capture.
	sawPartial bool

	// armedForWhere is set after a partial-index `where` keyword is emitted.
	// The next token the lexer produces is the predicate, captured verbatim
	// as a single TokString (see captureWherePredicate).
	armedForWhere bool

	// pending holds tokens already produced but not yet returned to the
	// caller. Raw-SQL capture needs to emit LBRACE, then BODY, then
	// continue scanning past the closing `}` — using a small queue keeps
	// the next() call honest as a single-token producer.
	pending []Token
}

// NewLexer constructs a Lexer over src. file is purely for error messages.
func NewLexer(file string, src []byte) *Lexer {
	return &Lexer{
		src:  src,
		file: file,
		line: 1,
		col:  1,
	}
}

// Lex returns every token until and including TokEOF.
// Errors are returned inline as TokError; the caller decides whether to stop.
func (l *Lexer) Lex() []Token {
	var out []Token
	for {
		t := l.next()
		out = append(out, t)
		if t.Kind == TokEOF {
			return out
		}
	}
}

// next produces the next token. Always advances past whitespace and comments
// before reading.
func (l *Lexer) next() Token {
	if len(l.pending) > 0 {
		t := l.pending[0]
		l.pending = l.pending[1:]
		return t
	}
	if l.armedForWhere {
		l.armedForWhere = false
		return l.captureWherePredicate()
	}
	l.skipWhitespaceAndComments()
	if l.pos >= len(l.src) {
		return l.tok(TokEOF, "")
	}

	start := l.position()
	r, size := l.peekRune()

	switch {
	case r == '{':
		l.advance(size)
		lbrace := Token{Kind: TokLBrace, Value: "{", Pos: start}
		if l.armedForRawSQL {
			// The lexer is positioned just past the opening `{` of a raw
			// SQL block. Capture the body verbatim (with SQL-string-aware
			// brace counting) and queue (LBRACE → BODY) so the parser
			// reads them in order. The closing `}` is left to the regular
			// lexer pass — its position carries the byte offset the AST
			// records as SQLBlock.EndPos.
			l.armedForRawSQL = false
			body, ok := l.captureRawSQLBody(start)
			if !ok {
				return Token{Kind: TokError, Value: "unterminated raw SQL block (unbalanced braces or EOF inside string)", Pos: start}
			}
			l.pending = append(l.pending, body)
			return lbrace
		}
		return lbrace
	case r == '}':
		l.advance(size)
		l.sawPartial = false
		return Token{Kind: TokRBrace, Value: "}", Pos: start}
	case r == '(':
		l.advance(size)
		return Token{Kind: TokLParen, Value: "(", Pos: start}
	case r == ')':
		l.advance(size)
		return Token{Kind: TokRParen, Value: ")", Pos: start}
	case r == '[':
		l.advance(size)
		return Token{Kind: TokLBracket, Value: "[", Pos: start}
	case r == ']':
		l.advance(size)
		return Token{Kind: TokRBracket, Value: "]", Pos: start}
	case r == ',':
		l.advance(size)
		return Token{Kind: TokComma, Value: ",", Pos: start}
	case r == ':':
		l.advance(size)
		if l.pos < len(l.src) && l.src[l.pos] == ':' {
			l.advance(1)
			return Token{Kind: TokDColon, Value: "::", Pos: start}
		}
		return Token{Kind: TokColon, Value: ":", Pos: start}
	case r == '=':
		l.advance(size)
		return Token{Kind: TokEquals, Value: "=", Pos: start}
	case r == '<':
		l.advance(size)
		if l.pos < len(l.src) && l.src[l.pos] == '=' {
			l.advance(1)
			return Token{Kind: TokLE, Value: "<=", Pos: start}
		}
		return Token{Kind: TokLT, Value: "<", Pos: start}
	case r == '>':
		l.advance(size)
		if l.pos < len(l.src) && l.src[l.pos] == '=' {
			l.advance(1)
			return Token{Kind: TokGE, Value: ">=", Pos: start}
		}
		return Token{Kind: TokGT, Value: ">", Pos: start}
	case r == '!':
		l.advance(size)
		if l.pos < len(l.src) && l.src[l.pos] == '=' {
			l.advance(1)
			return Token{Kind: TokNotEq, Value: "!=", Pos: start}
		}
		return Token{Kind: TokError, Value: "expected '!=' (lone '!' is not a token)", Pos: start}
	case r == '.':
		l.advance(size)
		return Token{Kind: TokDot, Value: ".", Pos: start}
	case r == '$':
		// `$name` is the placeholder shape used in DSL custom-query
		// constructs: `where consumer_id = $account_id`. The Value
		// excludes the leading `$` (so downstream code reads the bare
		// argument name) but the source position points at the `$`.
		// Inside raw `sql { ... }` blocks the lexer is bypassed —
		// `$name` lands in the captured raw string and pg_query_go
		// reads it as a PG parameter marker; this token is for typed
		// procedure steps and predicate expressions only.
		l.advance(size)
		if l.pos >= len(l.src) {
			return Token{Kind: TokError, Value: "expected identifier after '$'", Pos: start}
		}
		nameStart := l.pos
		if rn, _ := l.peekRune(); !isIdentStart(rn) {
			return Token{Kind: TokError, Value: "expected identifier after '$'", Pos: start}
		}
		for l.pos < len(l.src) {
			rn, sz := l.peekRune()
			if !isIdentRune(rn) {
				break
			}
			l.advance(sz)
		}
		return Token{Kind: TokArgPlaceholder, Value: string(l.src[nameStart:l.pos]), Pos: start}
	case r == '"':
		return l.scanString(start)
	case isDigit(r):
		return l.scanIntOrDuration(start)
	case isIdentStart(r):
		return l.scanIdentOrKeyword(start)
	}

	l.advance(size)
	return Token{Kind: TokError, Value: fmt.Sprintf("unexpected character %q", r), Pos: start}
}

func (l *Lexer) skipWhitespaceAndComments() {
	for l.pos < len(l.src) {
		r, size := l.peekRune()
		switch {
		case r == ' ' || r == '\t' || r == '\r':
			l.advance(size)
		case r == '\n':
			l.advance(size)
		case r == '/' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '/':
			// line comment
			for l.pos < len(l.src) && l.src[l.pos] != '\n' {
				l.advance(1)
			}
		case r == '/' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '*':
			// block comment — non-nesting per spec
			l.advance(2)
			for l.pos < len(l.src) {
				if l.src[l.pos] == '*' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '/' {
					l.advance(2)
					break
				}
				l.advance(1)
			}
		default:
			return
		}
	}
}

func (l *Lexer) scanString(start Position) Token {
	// src[l.pos] is known to be '"'.
	l.advance(1)
	var buf []byte
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c == '"' {
			l.advance(1)
			return Token{Kind: TokString, Value: string(buf), Pos: start}
		}
		if c == '\\' {
			if l.pos+1 >= len(l.src) {
				l.advance(1)
				return Token{Kind: TokError, Value: "unterminated escape in string", Pos: start}
			}
			esc := l.src[l.pos+1]
			switch esc {
			case '"', '\\':
				buf = append(buf, esc)
			case 'n':
				buf = append(buf, '\n')
			case 't':
				buf = append(buf, '\t')
			default:
				return Token{Kind: TokError, Value: fmt.Sprintf("unknown escape \\%c", esc), Pos: start}
			}
			l.advance(2)
			continue
		}
		if c == '\n' {
			return Token{Kind: TokError, Value: "unterminated string (newline before closing quote)", Pos: start}
		}
		buf = append(buf, c)
		l.advance(1)
	}
	return Token{Kind: TokError, Value: "unterminated string (EOF)", Pos: start}
}

func (l *Lexer) scanIntOrDuration(start Position) Token {
	digitStart := l.pos
	for l.pos < len(l.src) && isDigit(rune(l.src[l.pos])) {
		l.advance(1)
	}
	digits := string(l.src[digitStart:l.pos])

	// Float: a single `.` followed by at least one digit promotes the run to a
	// float (`3.14`). A `.` *not* followed by a digit stays a separate TokDot
	// (qualified refs like `vendor.Product` are unaffected — `Product` isn't a
	// digit). Index-predicate literals are the only place floats appear today.
	if l.pos+1 < len(l.src) && l.src[l.pos] == '.' && isDigit(rune(l.src[l.pos+1])) {
		l.advance(1) // consume '.'
		fracStart := l.pos
		for l.pos < len(l.src) && isDigit(rune(l.src[l.pos])) {
			l.advance(1)
		}
		return Token{Kind: TokFloat, Value: digits + "." + string(l.src[fracStart:l.pos]), Pos: start}
	}

	// Optional duration suffix: s / m / h / d
	if l.pos < len(l.src) {
		c := l.src[l.pos]
		switch c {
		case 's', 'm', 'h', 'd':
			// Only treat as duration if the next char isn't an identifier rune
			// (so `10mins` is "10" + "mins", not "10m" + "ins").
			next := l.pos + 1
			if next >= len(l.src) || !isIdentRune(rune(l.src[next])) {
				l.advance(1)
				return Token{Kind: TokDuration, Value: digits + string(c), Pos: start}
			}
		}
	}
	return Token{Kind: TokInt, Value: digits, Pos: start}
}

func (l *Lexer) scanIdentOrKeyword(start Position) Token {
	identStart := l.pos
	for l.pos < len(l.src) {
		r, size := l.peekRune()
		if !isIdentRune(r) {
			break
		}
		l.advance(size)
	}
	text := string(l.src[identStart:l.pos])
	if kw, ok := keywords[text]; ok {
		// Arm the raw-SQL capture so the next `{` enters body-verbatim
		// mode. `touches` appears in the grammar only as the argument-list
		// prefix on a raw SQL block, `sql touches(...) { ... }`, so the
		// lexer needs no parser state to recognise it.
		if kw == TokTouches {
			l.armedForRawSQL = true
		}
		// Arm raw capture of a partial-index `where` predicate, a SQL
		// expression over the full operator surface — `*`, `~`, `||` — that
		// the regular scanner would take as tokens.
		//
		// `where` also occurs in cache-invalidate and typed update/delete
		// steps, so capture arms only after a `partial`. TokPartial comes
		// only from `index partial` / `unique index partial`, whose field
		// list is always followed by the predicate `where`.
		if kw == TokPartial {
			l.sawPartial = true
		}
		if kw == TokWhere && l.sawPartial {
			l.armedForWhere = true
			l.sawPartial = false
		}
		return Token{Kind: kw, Value: text, Pos: start}
	}
	return Token{Kind: TokIdent, Value: text, Pos: start}
}

// captureWherePredicate captures a partial-index predicate verbatim as a single
// TokString. The predicate is a SQL boolean expression (the full operator
// surface), so it is captured raw rather than tokenized. It runs from just after
// `where` to the first member terminator — a newline, the entity-closing `}`, a
// `//` line comment, or EOF — encountered OUTSIDE a DSL `"..."` string. (`}` and
// `//` inside a string are part of the string.) Leading and trailing horizontal
// whitespace is trimmed. The DSL→SQL conversion happens later, at IR lowering.
func (l *Lexer) captureWherePredicate() Token {
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c == ' ' || c == '\t' || c == '\r' {
			l.advance(1)
			continue
		}
		break
	}
	start := l.position()
	bodyStart := l.pos
loop:
	for l.pos < len(l.src) {
		switch c := l.src[l.pos]; {
		case c == '\n', c == '}':
			break loop
		case c == '/' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '/':
			break loop
		case c == '"':
			if !l.skipDSLString() {
				break loop // unterminated; let validation surface the error
			}
		default:
			l.advance(1)
		}
	}
	body := strings.TrimRight(string(l.src[bodyStart:l.pos]), " \t\r")
	return Token{Kind: TokString, Value: body, Pos: start}
}

// skipDSLString advances past a DSL "..."-delimited string (cursor on the
// opening quote), honoring `\"`/`\\` escapes. Returns false if the string is
// unterminated before a newline or EOF (DSL strings are single-line).
func (l *Lexer) skipDSLString() bool {
	l.advance(1) // opening quote
	for l.pos < len(l.src) {
		switch l.src[l.pos] {
		case '\\':
			l.advance(2)
		case '"':
			l.advance(1)
			return true
		case '\n':
			return false
		default:
			l.advance(1)
		}
	}
	return false
}

// captureRawSQLBody consumes bytes verbatim from the current position to the
// `}` closing the raw SQL block at brace depth zero.
//
// The returned token's Value is the body without the surrounding braces, and
// its position is start, the opening `{`. The cursor is left on the matching
// `}` for the regular scanner to pick up as the next token.
//
// Braces are ignored inside single-quoted strings, double-quoted identifiers,
// `--` line comments and `/* */` block comments. PG block comments nest, so
// their depth is counted.
//
// Dollar-quoted strings, `$tag$ ... $tag$`, are not recognised: a brace inside
// one is counted, and an unbalanced result surfaces as a parse error carrying
// the opening brace's position.
//
// Returns (token, false) on EOF before the matching `}`.
func (l *Lexer) captureRawSQLBody(start Position) (Token, bool) {
	bodyStart := l.pos
	depth := 1 // we already consumed the opening `{`
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		switch c {
		case '{':
			depth++
			l.advance(1)
		case '}':
			depth--
			if depth == 0 {
				body := string(l.src[bodyStart:l.pos])
				return Token{Kind: TokString, Value: body, Pos: start}, true
			}
			l.advance(1)
		case '\'':
			if !l.skipSQLString('\'') {
				return Token{}, false
			}
		case '"':
			if !l.skipSQLString('"') {
				return Token{}, false
			}
		case '-':
			// SQL line comment: `--` to end of line. Anything else just
			// advances one byte and stays in scan mode.
			if l.pos+1 < len(l.src) && l.src[l.pos+1] == '-' {
				for l.pos < len(l.src) && l.src[l.pos] != '\n' {
					l.advance(1)
				}
			} else {
				l.advance(1)
			}
		case '/':
			if l.pos+1 < len(l.src) && l.src[l.pos+1] == '*' {
				if !l.skipSQLBlockComment() {
					return Token{}, false
				}
			} else {
				l.advance(1)
			}
		default:
			l.advance(1)
		}
	}
	return Token{}, false
}

// skipSQLString consumes a single- or double-quoted SQL string starting
// at the current quote character. `”` (resp. `""`) is the in-string
// escape for the same quote, per the SQL spec. Returns false on EOF
// before the closing quote, so the caller can surface an unterminated-
// string error pointing at the raw SQL block's opening `{`.
func (l *Lexer) skipSQLString(quote byte) bool {
	l.advance(1) // consume opening quote
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c == quote {
			// Doubled-quote = embedded literal of the quote character.
			if l.pos+1 < len(l.src) && l.src[l.pos+1] == quote {
				l.advance(2)
				continue
			}
			l.advance(1) // consume closing quote
			return true
		}
		l.advance(1)
	}
	return false
}

// skipSQLBlockComment consumes a /* ... */ comment. PG block comments
// nest, so depth is tracked. Returns false on EOF before the closing
// `*/`.
func (l *Lexer) skipSQLBlockComment() bool {
	l.advance(2) // consume the opening /*
	depth := 1
	for l.pos < len(l.src) {
		if l.pos+1 < len(l.src) && l.src[l.pos] == '/' && l.src[l.pos+1] == '*' {
			depth++
			l.advance(2)
			continue
		}
		if l.pos+1 < len(l.src) && l.src[l.pos] == '*' && l.src[l.pos+1] == '/' {
			depth--
			l.advance(2)
			if depth == 0 {
				return true
			}
			continue
		}
		l.advance(1)
	}
	return false
}

func (l *Lexer) peekRune() (rune, int) {
	if l.pos >= len(l.src) {
		return 0, 0
	}
	r, size := utf8.DecodeRune(l.src[l.pos:])
	return r, size
}

// advance moves the cursor forward by size bytes, updating line/col.
// size must correspond to a complete rune (or be 1 for ASCII fast paths).
func (l *Lexer) advance(size int) {
	for range size {
		if l.pos >= len(l.src) {
			return
		}
		if l.src[l.pos] == '\n' {
			l.prevCol = l.col
			l.line++
			l.col = 1
		} else {
			l.col++
		}
		l.pos++
	}
}

func (l *Lexer) position() Position {
	return Position{File: l.file, Line: l.line, Col: l.col, Byte: l.pos}
}

func (l *Lexer) tok(kind TokenKind, value string) Token {
	return Token{Kind: kind, Value: value, Pos: l.position()}
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }

func isIdentStart(r rune) bool {
	return r == '_' || unicode.IsLetter(r)
}

func isIdentRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// IsIdentifier reports whether s would lex as a single identifier token.
//
// Exported because schema generation has to answer exactly this question about
// a name it derived from a Postgres table, and answering it with a
// hand-rolled approximation got it wrong in both directions: an ASCII-only
// [A-Za-z0-9_] check accepted "2024Events" — a leading digit lexes as a
// number, so the generated file did not parse — and rejected "CaféOrders",
// which is a perfectly good identifier because isIdentStart is
// unicode.IsLetter rather than an ASCII range.
//
// One definition, so a caller cannot drift from the lexer that decides the
// real answer.
func IsIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if i == 0 {
			if !isIdentStart(r) {
				return false
			}
			continue
		}
		if !isIdentRune(r) {
			return false
		}
	}
	return true
}
