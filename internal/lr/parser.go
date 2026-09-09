package lr

import (
	"fmt"
)

// ---------- AST ----------

// Program is the parsed root: a list of statements.
type Program struct {
	Stmts []Stmt
}

// Stmt is implemented by all statement nodes.
type Stmt interface {
	stmtPos() Pos
}

type LetStmt struct {
	Pos   Pos
	Name  string
	Value Expr
}

func (s *LetStmt) stmtPos() Pos { return s.Pos }

type AssignStmt struct {
	Pos    Pos
	Target Expr // Ident, Index or Member
	Value  Expr
}

func (s *AssignStmt) stmtPos() Pos { return s.Pos }

type FnDecl struct {
	Pos    Pos
	Name   string
	Params []string
	Body   *BlockStmt
}

func (s *FnDecl) stmtPos() Pos { return s.Pos }

type IfStmt struct {
	Pos  Pos
	Cond Expr
	Then *BlockStmt
	Else Stmt // *BlockStmt or *IfStmt, may be nil
}

func (s *IfStmt) stmtPos() Pos { return s.Pos }

type WhileStmt struct {
	Pos  Pos
	Cond Expr
	Body *BlockStmt
}

func (s *WhileStmt) stmtPos() Pos { return s.Pos }

type ForStmt struct {
	Pos  Pos
	Var  string
	Iter Expr
	Body *BlockStmt
}

func (s *ForStmt) stmtPos() Pos { return s.Pos }

type BreakStmt struct{ Pos Pos }

func (s *BreakStmt) stmtPos() Pos { return s.Pos }

type ContinueStmt struct{ Pos Pos }

func (s *ContinueStmt) stmtPos() Pos { return s.Pos }

type ReturnStmt struct {
	Pos   Pos
	Value Expr // may be nil
}

func (s *ReturnStmt) stmtPos() Pos { return s.Pos }

type ExprStmt struct {
	Pos  Pos
	Expr Expr
}

func (s *ExprStmt) stmtPos() Pos { return s.Pos }

type BlockStmt struct {
	Pos   Pos
	Stmts []Stmt
}

func (s *BlockStmt) stmtPos() Pos { return s.Pos }

// Expr is implemented by all expression nodes.
type Expr interface {
	exprPos() Pos
}

type LitExpr struct {
	Pos Pos
	Val Value
}

func (e *LitExpr) exprPos() Pos { return e.Pos }

type IdentExpr struct {
	Pos  Pos
	Name string
}

func (e *IdentExpr) exprPos() Pos { return e.Pos }

type BinaryExpr struct {
	Pos   Pos
	Op    Tok
	Left  Expr
	Right Expr
}

func (e *BinaryExpr) exprPos() Pos { return e.Pos }

type UnaryExpr struct {
	Pos Pos
	Op  Tok
	X   Expr
}

func (e *UnaryExpr) exprPos() Pos { return e.Pos }

type CallExpr struct {
	Pos  Pos
	Fn   Expr
	Args []Expr
}

func (e *CallExpr) exprPos() Pos { return e.Pos }

type IndexExpr struct {
	Pos   Pos
	Obj   Expr
	Index Expr
}

func (e *IndexExpr) exprPos() Pos { return e.Pos }

type MemberExpr struct {
	Pos  Pos
	Obj  Expr
	Name string
}

func (e *MemberExpr) exprPos() Pos { return e.Pos }

type ListExpr struct {
	Pos   Pos
	Elems []Expr
}

func (e *ListExpr) exprPos() Pos { return e.Pos }

type MapExpr struct {
	Pos  Pos
	Keys []string
	Vals []Expr
}

func (e *MapExpr) exprPos() Pos { return e.Pos }

// ---------- Parser ----------

// ParseError is a syntax error with position.
type ParseError struct {
	Msg string
	Pos Pos
}

func (e *ParseError) Error() string { return fmt.Sprintf("syntax error: %s at %s", e.Msg, e.Pos) }

// Parse parses LRS source into a Program.
func Parse(src string) (*Program, error) {
	toks, err := Lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	prog, err := p.parseProgram()
	if err != nil {
		return nil, err
	}
	return prog, nil
}

type parser struct {
	toks []Token
	i    int
}

func (p *parser) peek() Token { return p.toks[p.i] }

func (p *parser) peek2() Token {
	if p.i+1 >= len(p.toks) {
		return p.toks[len(p.toks)-1]
	}
	return p.toks[p.i+1]
}

func (p *parser) next() Token {
	t := p.toks[p.i]
	if p.i < len(p.toks)-1 {
		p.i++
	}
	return t
}

func (p *parser) at(t Tok) bool { return p.peek().Type == t }

func (p *parser) expect(t Tok, what string) (Token, error) {
	if !p.at(t) {
		return Token{}, &ParseError{Msg: fmt.Sprintf("expected %s, found %s", what, describe(p.peek())), Pos: p.peek().Pos}
	}
	return p.next(), nil
}

func describe(t Token) string {
	if t.Type == T_EOF {
		return "end of file"
	}
	if t.Type == T_Ident || t.Type == T_Num || t.Type == T_Str {
		return fmt.Sprintf("%q", t.Text)
	}
	return fmt.Sprintf("'%s'", t.Text)
}

func (p *parser) parseProgram() (*Program, error) {
	prog := &Program{}
	for !p.at(T_EOF) {
		// Tolerate stray semicolons between statements.
		if p.at(T_Semi) {
			p.next()
			continue
		}
		s, err := p.parseStmt()
		if err != nil {
			return nil, err
		}
		prog.Stmts = append(prog.Stmts, s)
	}
	return prog, nil
}

func (p *parser) parseStmt() (Stmt, error) {
	switch p.peek().Type {
	case T_Let:
		return p.parseLet()
	case T_Fn:
		return p.parseFn()
	case T_If:
		return p.parseIf()
	case T_While:
		return p.parseWhile()
	case T_For:
		return p.parseFor()
	case T_Break:
		t := p.next()
		if _, err := p.expect(T_Semi, "';'"); err != nil {
			return nil, err
		}
		return &BreakStmt{Pos: t.Pos}, nil
	case T_Continue:
		t := p.next()
		if _, err := p.expect(T_Semi, "';'"); err != nil {
			return nil, err
		}
		return &ContinueStmt{Pos: t.Pos}, nil
	case T_Return:
		t := p.next()
		var v Expr
		if !p.at(T_Semi) {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			v = e
		}
		if _, err := p.expect(T_Semi, "';'"); err != nil {
			return nil, err
		}
		return &ReturnStmt{Pos: t.Pos, Value: v}, nil
	case T_LBrace:
		return p.parseBlock()
	default:
		return p.parseExprOrAssign()
	}
}

func (p *parser) parseLet() (Stmt, error) {
	t := p.next() // let
	name, err := p.expect(T_Ident, "variable name")
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(T_Eq, "'='"); err != nil {
		return nil, err
	}
	v, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(T_Semi, "';'"); err != nil {
		return nil, err
	}
	return &LetStmt{Pos: t.Pos, Name: name.Text, Value: v}, nil
}

func (p *parser) parseFn() (Stmt, error) {
	t := p.next() // fn
	name, err := p.expect(T_Ident, "function name")
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(T_LParen, "'('"); err != nil {
		return nil, err
	}
	var params []string
	if !p.at(T_RParen) {
		for {
			id, err := p.expect(T_Ident, "parameter name")
			if err != nil {
				return nil, err
			}
			params = append(params, id.Text)
			if p.at(T_Comma) {
				p.next()
				continue
			}
			break
		}
	}
	if _, err := p.expect(T_RParen, "')'"); err != nil {
		return nil, err
	}
	body, err := p.parseBlock()
	if err != nil {
		return nil, err
	}
	return &FnDecl{Pos: t.Pos, Name: name.Text, Params: params, Body: body}, nil
}

func (p *parser) parseIf() (Stmt, error) {
	t := p.next() // if
	cond, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	then, err := p.parseBlock()
	if err != nil {
		return nil, err
	}
	var els Stmt
	if p.at(T_Else) {
		p.next()
		if p.at(T_If) {
			els, err = p.parseIf()
			if err != nil {
				return nil, err
			}
		} else {
			els, err = p.parseBlock()
			if err != nil {
				return nil, err
			}
		}
	}
	return &IfStmt{Pos: t.Pos, Cond: cond, Then: then, Else: els}, nil
}

func (p *parser) parseWhile() (Stmt, error) {
	t := p.next() // while
	cond, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	body, err := p.parseBlock()
	if err != nil {
		return nil, err
	}
	return &WhileStmt{Pos: t.Pos, Cond: cond, Body: body}, nil
}

func (p *parser) parseFor() (Stmt, error) {
	t := p.next() // for
	v, err := p.expect(T_Ident, "loop variable")
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(T_In, "'in'"); err != nil {
		return nil, err
	}
	iter, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	body, err := p.parseBlock()
	if err != nil {
		return nil, err
	}
	return &ForStmt{Pos: t.Pos, Var: v.Text, Iter: iter, Body: body}, nil
}

func (p *parser) parseBlock() (*BlockStmt, error) {
	t, err := p.expect(T_LBrace, "'{'")
	if err != nil {
		return nil, err
	}
	b := &BlockStmt{Pos: t.Pos}
	for !p.at(T_RBrace) && !p.at(T_EOF) {
		if p.at(T_Semi) {
			p.next()
			continue
		}
		s, err := p.parseStmt()
		if err != nil {
			return nil, err
		}
		b.Stmts = append(b.Stmts, s)
	}
	if _, err := p.expect(T_RBrace, "'}'"); err != nil {
		return nil, err
	}
	return b, nil
}

// parseExprOrAssign parses `target = expr ;` or a bare expression statement.
func (p *parser) parseExprOrAssign() (Stmt, error) {
	start := p.peek().Pos
	e, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if p.at(T_Eq) {
		switch e.(type) {
		case *IdentExpr, *IndexExpr, *MemberExpr:
		default:
			return nil, &ParseError{Msg: "cannot assign to this expression", Pos: start}
		}
		eq := p.next()
		v, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(T_Semi, "';'"); err != nil {
			return nil, err
		}
		return &AssignStmt{Pos: eq.Pos, Target: e, Value: v}, nil
	}
	if _, err := p.expect(T_Semi, "';'"); err != nil {
		return nil, err
	}
	return &ExprStmt{Pos: start, Expr: e}, nil
}

// ---------- Expressions (precedence climbing) ----------

func (p *parser) parseExpr() (Expr, error) { return p.parseOr() }

func (p *parser) parseOr() (Expr, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.at(T_PipePipe) || p.at(T_Or) {
		op := p.next()
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = &BinaryExpr{Pos: op.Pos, Op: op.Type, Left: left, Right: right}
	}
	return left, nil
}

func (p *parser) parseAnd() (Expr, error) {
	left, err := p.parseEquality()
	if err != nil {
		return nil, err
	}
	for p.at(T_AmpAmp) || p.at(T_And) {
		op := p.next()
		right, err := p.parseEquality()
		if err != nil {
			return nil, err
		}
		left = &BinaryExpr{Pos: op.Pos, Op: op.Type, Left: left, Right: right}
	}
	return left, nil
}

func (p *parser) parseEquality() (Expr, error) {
	left, err := p.parseComparison()
	if err != nil {
		return nil, err
	}
	for p.at(T_EqEq) || p.at(T_BangEq) {
		op := p.next()
		right, err := p.parseComparison()
		if err != nil {
			return nil, err
		}
		left = &BinaryExpr{Pos: op.Pos, Op: op.Type, Left: left, Right: right}
	}
	return left, nil
}

func (p *parser) parseComparison() (Expr, error) {
	left, err := p.parseAdd()
	if err != nil {
		return nil, err
	}
	for p.at(T_Lt) || p.at(T_LtEq) || p.at(T_Gt) || p.at(T_GtEq) || p.at(T_In) {
		op := p.next()
		right, err := p.parseAdd()
		if err != nil {
			return nil, err
		}
		left = &BinaryExpr{Pos: op.Pos, Op: op.Type, Left: left, Right: right}
	}
	return left, nil
}

func (p *parser) parseAdd() (Expr, error) {
	left, err := p.parseMul()
	if err != nil {
		return nil, err
	}
	for p.at(T_Plus) || p.at(T_Minus) {
		op := p.next()
		right, err := p.parseMul()
		if err != nil {
			return nil, err
		}
		left = &BinaryExpr{Pos: op.Pos, Op: op.Type, Left: left, Right: right}
	}
	return left, nil
}

func (p *parser) parseMul() (Expr, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for p.at(T_Star) || p.at(T_Slash) || p.at(T_Percent) {
		op := p.next()
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		left = &BinaryExpr{Pos: op.Pos, Op: op.Type, Left: left, Right: right}
	}
	return left, nil
}

func (p *parser) parseUnary() (Expr, error) {
	if p.at(T_Minus) || p.at(T_Bang) || p.at(T_Not) {
		op := p.next()
		x, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &UnaryExpr{Pos: op.Pos, Op: op.Type, X: x}, nil
	}
	return p.parsePostfix()
}

func (p *parser) parsePostfix() (Expr, error) {
	e, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	for {
		switch {
		case p.at(T_LParen):
			lp := p.next()
			var args []Expr
			if !p.at(T_RParen) {
				for {
					a, err := p.parseExpr()
					if err != nil {
						return nil, err
					}
					args = append(args, a)
					if p.at(T_Comma) {
						p.next()
						continue
					}
					break
				}
			}
			if _, err := p.expect(T_RParen, "')'"); err != nil {
				return nil, err
			}
			e = &CallExpr{Pos: lp.Pos, Fn: e, Args: args}
		case p.at(T_LBrack):
			lb := p.next()
			idx, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			if _, err := p.expect(T_RBrack, "']'"); err != nil {
				return nil, err
			}
			e = &IndexExpr{Pos: lb.Pos, Obj: e, Index: idx}
		case p.at(T_Dot):
			d := p.next()
			id, err := p.expect(T_Ident, "field name after '.'")
			if err != nil {
				return nil, err
			}
			e = &MemberExpr{Pos: d.Pos, Obj: e, Name: id.Text}
		default:
			return e, nil
		}
	}
}

func (p *parser) parsePrimary() (Expr, error) {
	t := p.peek()
	switch t.Type {
	case T_Num:
		p.next()
		return &LitExpr{Pos: t.Pos, Val: Num(t.Num)}, nil
	case T_Str:
		p.next()
		return &LitExpr{Pos: t.Pos, Val: Str(t.Text)}, nil
	case T_True:
		p.next()
		return &LitExpr{Pos: t.Pos, Val: True}, nil
	case T_False:
		p.next()
		return &LitExpr{Pos: t.Pos, Val: False}, nil
	case T_Null:
		p.next()
		return &LitExpr{Pos: t.Pos, Val: Null}, nil
	case T_Ident:
		p.next()
		return &IdentExpr{Pos: t.Pos, Name: t.Text}, nil
	case T_LParen:
		p.next()
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(T_RParen, "')'"); err != nil {
			return nil, err
		}
		return e, nil
	case T_LBrack:
		return p.parseList()
	case T_LBrace:
		return p.parseMap()
	default:
		return nil, &ParseError{Msg: fmt.Sprintf("unexpected %s", describe(t)), Pos: t.Pos}
	}
}

func (p *parser) parseList() (Expr, error) {
	lb := p.next() // [
	e := &ListExpr{Pos: lb.Pos}
	if p.at(T_RBrack) {
		p.next()
		return e, nil
	}
	for {
		el, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		e.Elems = append(e.Elems, el)
		if p.at(T_Comma) {
			p.next()
			if p.at(T_RBrack) {
				p.next()
				return e, nil
			}
			continue
		}
		break
	}
	if _, err := p.expect(T_RBrack, "']'"); err != nil {
		return nil, err
	}
	return e, nil
}

func (p *parser) parseMap() (Expr, error) {
	lb := p.next() // {
	e := &MapExpr{Pos: lb.Pos}
	if p.at(T_RBrace) {
		p.next()
		return e, nil
	}
	for {
		kt := p.peek()
		var key string
		switch kt.Type {
		case T_Ident, T_Str:
			key = kt.Text
			p.next()
		default:
			return nil, &ParseError{Msg: fmt.Sprintf("expected map key, found %s", describe(kt)), Pos: kt.Pos}
		}
		if _, err := p.expect(T_Colon, "':' after map key"); err != nil {
			return nil, err
		}
		v, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		e.Keys = append(e.Keys, key)
		e.Vals = append(e.Vals, v)
		if p.at(T_Comma) {
			p.next()
			if p.at(T_RBrace) {
				p.next()
				return e, nil
			}
			continue
		}
		break
	}
	if _, err := p.expect(T_RBrace, "'}'"); err != nil {
		return nil, err
	}
	return e, nil
}
