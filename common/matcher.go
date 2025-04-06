package common

import (
	"fmt"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/IGLOU-EU/go-wildcard/v2"
	"github.com/erpc/erpc/telemetry"
)

type tokenType int

const (
	tokenOr tokenType = iota
	tokenAnd
	tokenNot
	tokenLParen
	tokenRParen
	tokenPattern
)

type token struct {
	typ   tokenType
	value string
}

type parser struct {
	tokens []token
	pos    int
}

func WildcardMatch(pattern, value string) (bool, error) {
	tokens, err := tokenize(pattern)
	if err != nil {
		return false, err
	}

	p := &parser{tokens: tokens}
	matcher, err := p.parseExpression()
	if err != nil {
		return false, err
	}

	return matcher(value), nil
}

// ValidatePattern checks if a pattern string is syntactically valid.
// It returns an error if the pattern is invalid, nil otherwise.
func ValidatePattern(pattern string) error {
	if pattern == "" {
		return nil
	}

	defer func() {
		if r := recover(); r != nil {
			telemetry.MetricUnexpectedPanicTotal.WithLabelValues(
				"validate-pattern",
				fmt.Sprintf("pattern:%s", pattern),
				ErrorFingerprint(r),
			).Inc()
			// Convert panic into error
			if err, ok := r.(string); ok {
				panic(fmt.Errorf("invalid pattern syntax: %s -> %s", err, string(debug.Stack())))
			}
			panic(r) // re-panic if it's not our error
		}
	}()

	tokens, err := tokenize(pattern)
	if err != nil {
		return err
	}

	// Check for empty pattern that isn't explicitly "<empty>"
	if len(tokens) == 0 {
		return fmt.Errorf("empty pattern")
	}

	// Validate token sequence
	parenCount := 0
	for i, tok := range tokens {
		switch tok.typ {
		case tokenLParen:
			parenCount++
		case tokenRParen:
			parenCount--
			if parenCount < 0 {
				return fmt.Errorf("unmatched closing parenthesis at position %d", i)
			}
		case tokenOr, tokenAnd:
			// Check if operator has operands on both sides
			if i == 0 || i == len(tokens)-1 {
				return fmt.Errorf("operator '%s' missing operand at position %d", tok.value, i)
			}
			prev := tokens[i-1]
			next := tokens[i+1]
			if prev.typ != tokenPattern && prev.typ != tokenRParen {
				return fmt.Errorf("invalid left operand for '%s' at position %d", tok.value, i)
			}
			if next.typ != tokenPattern && next.typ != tokenLParen && next.typ != tokenNot {
				return fmt.Errorf("invalid right operand for '%s' at position %d", tok.value, i)
			}
		case tokenNot:
			// Check if NOT has an operand
			if i == len(tokens)-1 {
				return fmt.Errorf("NOT operator missing operand at position %d", i)
			}
			next := tokens[i+1]
			if next.typ != tokenPattern && next.typ != tokenLParen {
				return fmt.Errorf("invalid operand for NOT at position %d", i)
			}
		case tokenPattern:
			// Validate numeric comparisons
			if strings.HasPrefix(tok.value, ">=") || strings.HasPrefix(tok.value, "<=") {
				val := tok.value[2:]
				if !isValidHexNumber(val) {
					return fmt.Errorf("invalid hex number in comparison: %s", tok.value)
				}
			} else if strings.HasPrefix(tok.value, ">") || strings.HasPrefix(tok.value, "<") || strings.HasPrefix(tok.value, "=") {
				val := tok.value[1:]
				if !isValidHexNumber(val) {
					return fmt.Errorf("invalid hex number in comparison: %s", tok.value)
				}
			}
		}
	}

	if parenCount > 0 {
		return fmt.Errorf("unclosed parenthesis")
	}

	// Try parsing the pattern to ensure it's valid
	p := &parser{tokens: tokens}
	_, err = p.parseExpression()
	if err != nil {
		return err
	}

	if p.pos < len(tokens) {
		return fmt.Errorf("unexpected token at position %d: %s", p.pos, tokens[p.pos].value)
	}

	return nil
}

// isValidHexNumber checks if a string is a valid hex number
func isValidHexNumber(s string) bool {
	s = strings.TrimPrefix(s, "0x")
	_, err := strconv.ParseUint(s, 16, 64)
	return err == nil
}

// Tokenizer
func tokenize(pattern string) ([]token, error) {
	var tokens []token
	var current strings.Builder

	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '|':
			if current.Len() > 0 {
				tokens = append(tokens, token{tokenPattern, strings.TrimSpace(current.String())})
				current.Reset()
			}
			tokens = append(tokens, token{tokenOr, "|"})
		case '&':
			if current.Len() > 0 {
				tokens = append(tokens, token{tokenPattern, strings.TrimSpace(current.String())})
				current.Reset()
			}
			tokens = append(tokens, token{tokenAnd, "&"})
		case '!':
			if current.Len() > 0 {
				tokens = append(tokens, token{tokenPattern, strings.TrimSpace(current.String())})
				current.Reset()
			}
			tokens = append(tokens, token{tokenNot, "!"})
		case '(':
			if current.Len() > 0 {
				tokens = append(tokens, token{tokenPattern, strings.TrimSpace(current.String())})
				current.Reset()
			}
			tokens = append(tokens, token{tokenLParen, "("})
		case ')':
			if current.Len() > 0 {
				tokens = append(tokens, token{tokenPattern, strings.TrimSpace(current.String())})
				current.Reset()
			}
			tokens = append(tokens, token{tokenRParen, ")"})
		case ' ':
			if current.Len() > 0 {
				tokens = append(tokens, token{tokenPattern, strings.TrimSpace(current.String())})
				current.Reset()
			}
		default:
			current.WriteByte(pattern[i])
		}
	}

	if current.Len() > 0 {
		tokens = append(tokens, token{tokenPattern, strings.TrimSpace(current.String())})
	}

	return tokens, nil
}

// Parser
func (p *parser) current() token {
	if p.pos >= len(p.tokens) {
		return token{tokenPattern, ""}
	}
	return p.tokens[p.pos]
}

func (p *parser) advance() {
	p.pos++
}

func (p *parser) parseExpression() (func(string) bool, error) {
	return p.parseOr()
}

func (p *parser) parseOr() (func(string) bool, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}

	for p.pos < len(p.tokens) && p.current().typ == tokenOr {
		p.advance() // consume OR
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		prev := left
		left = func(s string) bool {
			return prev(s) || right(s)
		}
	}

	return left, nil
}

func (p *parser) parseAnd() (func(string) bool, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}

	for p.pos < len(p.tokens) && p.current().typ == tokenAnd {
		p.advance() // consume AND
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		prev := left
		left = func(s string) bool {
			return prev(s) && right(s)
		}
	}

	return left, nil
}

func (p *parser) parseUnary() (func(string) bool, error) {
	if p.current().typ == tokenNot {
		p.advance() // consume NOT
		operand, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return func(s string) bool {
			return !operand(s)
		}, nil
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() (func(string) bool, error) {
	if p.current().typ == tokenLParen {
		p.advance() // consume (
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		if p.current().typ != tokenRParen {
			return nil, fmt.Errorf("expected closing parenthesis")
		}
		p.advance() // consume )
		return expr, nil
	}

	if p.current().typ == tokenPattern {
		pattern := p.current().value
		p.advance()
		return func(value string) bool {
			// Handle empty check
			if pattern == "<empty>" {
				return value == ""
			}

			// Trim spaces from value for numeric comparisons
			value = strings.TrimSpace(value)

			// Handle numeric comparisons for hex values
			if strings.HasPrefix(value, "0x") {
				if len(pattern) > 2 && strings.HasPrefix(pattern, ">=") {
					return compareNumbers(pattern[2:], value, ">=")
				}
				if len(pattern) > 2 && strings.HasPrefix(pattern, "<=") {
					return compareNumbers(pattern[2:], value, "<=")
				}
				if len(pattern) > 1 && strings.HasPrefix(pattern, ">") {
					return compareNumbers(pattern[1:], value, ">")
				}
				if len(pattern) > 1 && strings.HasPrefix(pattern, "<") {
					return compareNumbers(pattern[1:], value, "<")
				}
				if len(pattern) > 1 && strings.HasPrefix(pattern, "=") {
					return compareNumbers(pattern[1:], value, "=")
				}
			}

			return wildcard.Match(pattern, value)
		}, nil
	}

	return func(string) bool { return false }, nil
}

func compareNumbers(pattern, value, op string) bool {
	patternNum, err := parseNumber(pattern)
	if err != nil {
		return false
	}

	valueNum, err := parseNumber(value)
	if err != nil {
		return false
	}

	switch op {
	case ">=":
		return valueNum >= patternNum
	case "<=":
		return valueNum <= patternNum
	case ">":
		return valueNum > patternNum
	case "<":
		return valueNum < patternNum
	case "=":
		return valueNum == patternNum
	}
	return false
}

func parseNumber(s string) (int64, error) {
	// Try parsing as hex
	if strings.HasPrefix(strings.ToLower(s), "0x") {
		if num, err := strconv.ParseInt(s[2:], 16, 64); err == nil {
			return num, nil
		}
	}

	// Try parsing as decimal
	if num, err := strconv.ParseInt(s, 0, 64); err == nil {
		return num, nil
	}

	return 0, fmt.Errorf("unable to parse number: %s", s)
}
