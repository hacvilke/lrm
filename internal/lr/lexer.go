package lr

import (
	"fmt"
	"strconv"
	"strings"
)

// Tok is a lexical token type.
type Tok int

const (
	T_EOF Tok = iota
	T_Ident
	T_Num
	T_Str
	// Keywords.
	T_Let
	T_Fn
	T_If
	T_Else
	T_While
	T_For
	T_In
	T_Break
	T_Continue
	T_Return
	T_True
	T_False
	T_Null
	T_And
	T_Or
	T_Not
	// Punctuation / operators.
	T_LParen
	T_RParen
	T_LBrack
	T_RBrack
	T_LBrace
	T_RBrace
	T_Comma
	T_Colon
	T_Semi
	T_Dot
	T_Plus
	T_Minus
	T_Star
	T_Slash
	T_Percent
	T_Eq
	T_EqEq
	T_Bang
	T_BangEq
	T_Lt
	T_LtEq
	T_Gt
	T_GtEq
	T_AmpAmp
	T_PipePipe
)

// Pos is a 1-based line/column position.
type Pos struct {
	Line int
	Col  int
}

func (p Pos) String() string { return fmt.Sprintf("line %d, col %d", p.Line, p.Col) }

// Token is one lexed unit with source position.
type Token struct {
	Type Tok
	Text string // identifier text, string contents, number literal
	Num  float64
	Pos  Pos
}

var keywords = map[string]Tok{
	"let": T_Let, "fn": T_Fn, "if": T_If, "else": T_Else,
	"while": T_While, "for": T_For, "in": T_In,
	"break": T_Break, "continue": T_Continue, "return": T_Return,
	"true": T_True, "false": T_False, "null": T_Null,
	"and": T_And, "or": T_Or, "not": T_Not,
}

// LexError is a lexical error with position.
type LexError struct {
	Msg string
	Pos Pos
}

func (e *LexError) Error() string { return fmt.Sprintf("%s at %s", e.Msg, e.Pos) }

// Lex tokenizes LRS source.
func Lex(src string) ([]Token, error) {
	l := &lexer{src: src, line: 1, col: 1}
	var out []Token
	for {
		t, err := l.next()
		if err != nil {
			return nil, err
		}
		out = append(out, t)
		if t.Type == T_EOF {
			return out, nil
		}
	}
}

type lexer struct {
	src  string
	i    int
	line int
	col  int
}

func (l *lexer) pos() Pos { return Pos{Line: l.line, Col: l.col} }

func (l *lexer) peek() byte {
	if l.i >= len(l.src) {
		return 0
	}
	return l.src[l.i]
}

func (l *lexer) peek2() byte {
	if l.i+1 >= len(l.src) {
		return 0
	}
	return l.src[l.i+1]
}

func (l *lexer) advance() byte {
	c := l.src[l.i]
	l.i++
	if c == '\n' {
		l.line++
		l.col = 1
	} else {
		l.col++
	}
	return c
}

func (l *lexer) next() (Token, error) {
	// Skip whitespace and comments.
	for l.i < len(l.src) {
		c := l.peek()
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			l.advance()
			continue
		}
		if c == '/' && l.peek2() == '/' {
			for l.i < len(l.src) && l.peek() != '\n' {
				l.advance()
			}
			continue
		}
		if c == '/' && l.peek2() == '*' {
			l.advance()
			l.advance()
			depth := 1
			for l.i < len(l.src) && depth > 0 {
				if l.peek() == '/' && l.peek2() == '*' {
					l.advance()
					l.advance()
					depth++
					continue
				}
				if l.peek() == '*' && l.peek2() == '/' {
					l.advance()
					l.advance()
					depth--
					continue
				}
				l.advance()
			}
			if depth > 0 {
				return Token{}, &LexError{Msg: "unterminated /* comment", Pos: l.pos()}
			}
			continue
		}
		break
	}
	if l.i >= len(l.src) {
		return Token{Type: T_EOF, Pos: l.pos()}, nil
	}
	p := l.pos()
	c := l.advance()
	switch {
	case isLetter(c) || c == '_':
		start := l.i - 1
		for l.i < len(l.src) && (isLetter(l.peek()) || isDigit(l.peek()) || l.peek() == '_') {
			l.advance()
		}
		word := l.src[start:l.i]
		if kw, ok := keywords[word]; ok {
			return Token{Type: kw, Text: word, Pos: p}, nil
		}
		return Token{Type: T_Ident, Text: word, Pos: p}, nil
	case isDigit(c) || (c == '.' && isDigit(l.peek())):
		start := l.i - 1
		for l.i < len(l.src) && isDigit(l.peek()) {
			l.advance()
		}
		if l.peek() == '.' && isDigit(l.peek2()) {
			l.advance()
			for l.i < len(l.src) && isDigit(l.peek()) {
				l.advance()
			}
		}
		if l.peek() == 'e' || l.peek() == 'E' {
			j := l.i + 1
			if j < len(l.src) && (l.src[j] == '+' || l.src[j] == '-') {
				j++
			}
			if j < len(l.src) && isDigit(l.src[j]) {
				l.advance()
				if l.peek() == '+' || l.peek() == '-' {
					l.advance()
				}
				for l.i < len(l.src) && isDigit(l.peek()) {
					l.advance()
				}
			}
		}
		text := l.src[start:l.i]
		num, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return Token{}, &LexError{Msg: fmt.Sprintf("bad number %q", text), Pos: p}
		}
		return Token{Type: T_Num, Text: text, Num: num, Pos: p}, nil
	case c == '"' || c == '\'':
		quote := c
		var sb strings.Builder
		closed := false
		for l.i < len(l.src) {
			ch := l.advance()
			if ch == quote {
				closed = true
				break
			}
			if ch == '\n' {
				return Token{}, &LexError{Msg: "unterminated string (newline in string literal)", Pos: p}
			}
			if ch == '\\' {
				if l.i >= len(l.src) {
					break
				}
				esc := l.advance()
				switch esc {
				case 'n':
					sb.WriteByte('\n')
				case 't':
					sb.WriteByte('\t')
				case 'r':
					sb.WriteByte('\r')
				case '\\':
					sb.WriteByte('\\')
				case '"':
					sb.WriteByte('"')
				case '\'':
					sb.WriteByte('\'')
				default:
					return Token{}, &LexError{Msg: fmt.Sprintf("unknown escape \\%c", esc), Pos: l.pos()}
				}
				continue
			}
			sb.WriteByte(ch)
		}
		if !closed {
			return Token{}, &LexError{Msg: "unterminated string", Pos: p}
		}
		return Token{Type: T_Str, Text: sb.String(), Pos: p}, nil
	}
	// Two-char operators.
	two := string([]byte{c, l.peek()})
	switch two {
	case "==":
		l.advance()
		return Token{Type: T_EqEq, Text: "==", Pos: p}, nil
	case "!=":
		l.advance()
		return Token{Type: T_BangEq, Text: "!=", Pos: p}, nil
	case "<=":
		l.advance()
		return Token{Type: T_LtEq, Text: "<=", Pos: p}, nil
	case ">=":
		l.advance()
		return Token{Type: T_GtEq, Text: ">=", Pos: p}, nil
	case "&&":
		l.advance()
		return Token{Type: T_AmpAmp, Text: "&&", Pos: p}, nil
	case "||":
		l.advance()
		return Token{Type: T_PipePipe, Text: "||", Pos: p}, nil
	}
	switch c {
	case '(':
		return Token{Type: T_LParen, Text: "(", Pos: p}, nil
	case ')':
		return Token{Type: T_RParen, Text: ")", Pos: p}, nil
	case '[':
		return Token{Type: T_LBrack, Text: "[", Pos: p}, nil
	case ']':
		return Token{Type: T_RBrack, Text: "]", Pos: p}, nil
	case '{':
		return Token{Type: T_LBrace, Text: "{", Pos: p}, nil
	case '}':
		return Token{Type: T_RBrace, Text: "}", Pos: p}, nil
	case ',':
		return Token{Type: T_Comma, Text: ",", Pos: p}, nil
	case ':':
		return Token{Type: T_Colon, Text: ":", Pos: p}, nil
	case ';':
		return Token{Type: T_Semi, Text: ";", Pos: p}, nil
	case '.':
		return Token{Type: T_Dot, Text: ".", Pos: p}, nil
	case '+':
		return Token{Type: T_Plus, Text: "+", Pos: p}, nil
	case '-':
		return Token{Type: T_Minus, Text: "-", Pos: p}, nil
	case '*':
		return Token{Type: T_Star, Text: "*", Pos: p}, nil
	case '/':
		return Token{Type: T_Slash, Text: "/", Pos: p}, nil
	case '%':
		return Token{Type: T_Percent, Text: "%", Pos: p}, nil
	case '=':
		return Token{Type: T_Eq, Text: "=", Pos: p}, nil
	case '!':
		return Token{Type: T_Bang, Text: "!", Pos: p}, nil
	case '<':
		return Token{Type: T_Lt, Text: "<", Pos: p}, nil
	case '>':
		return Token{Type: T_Gt, Text: ">", Pos: p}, nil
	}
	return Token{}, &LexError{Msg: fmt.Sprintf("unexpected character %q", c), Pos: p}
}

func isLetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
func isDigit(c byte) bool  { return c >= '0' && c <= '9' }
