// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package parser

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	unimp "github.com/cockroachdb/cockroach/pkg/util/errorutil/unimplemented"
	"github.com/cockroachdb/errors"
)

type lexer struct {
	in string
	// tokens contains tokens generated by the scanner.
	tokens []sqlSymType

	// The type that should be used when an INT or SERIAL is encountered.
	nakedIntType *types.T

	// lastPos is the position into the tokens slice of the last
	// token returned by Lex().
	lastPos int

	stmt tree.Statement
	// numPlaceholders is 1 + the highest placeholder index encountered.
	numPlaceholders int
	numAnnotations  tree.AnnotationIdx

	lastError error
}

func (l *lexer) init(sql string, tokens []sqlSymType, nakedIntType *types.T) {
	l.in = sql
	l.tokens = tokens
	l.lastPos = -1
	l.stmt = nil
	l.numPlaceholders = 0
	l.numAnnotations = 0
	l.lastError = nil

	l.nakedIntType = nakedIntType
}

// cleanup is used to avoid holding on to memory unnecessarily (for the cases
// where we reuse a scanner).
func (l *lexer) cleanup() {
	l.tokens = nil
	l.stmt = nil
	l.lastError = nil
}

// Lex lexes a token from input.
func (l *lexer) Lex(lval *sqlSymType) int {
	l.lastPos++
	// The core lexing takes place in the scanner. Here we do a small bit of post
	// processing of the lexical tokens so that the grammar only requires
	// one-token lookahead despite SQL requiring multi-token lookahead in some
	// cases. These special cases are handled below and the returned tokens are
	// adjusted to reflect the lookahead (LA) that occurred.
	if l.lastPos >= len(l.tokens) {
		lval.id = 0
		lval.pos = int32(len(l.in))
		lval.str = "EOF"
		return 0
	}
	*lval = l.tokens[l.lastPos]

	switch lval.id {
	case NOTHING:
		// Introducing the "RETURNING NOTHING" syntax in CockroachDB
		// was a terrible idea, given that it is not even used any more!
		// We should really deprecate it and remove this special case.
		if l.lastPos > 0 && l.tokens[l.lastPos-1].id == RETURNING {
			lval.id = NOTHING_AFTER_RETURNING
		}
	case INDEX:
		// The following complex logic is a consternation, really.
		//
		// It flows from a profoundly mistaken decision to allow the INDEX
		// keyword inside the column definition list of CREATE, a place
		// where PostgreSQL did not allow it, for a very good reason:
		// applications legitimately want to name columns with the name
		// "index".
		//
		// After this mistaken decision was first made, the INDEX keyword
		// was also allowed in CockroachDB in another place where it is
		// partially ambiguous with other identifiers: ORDER BY
		// (`ORDER BY INDEX foo@bar`, ambiguous with `ORDER BY index`).
		//
		// Sadly it took a very long time before we realized this mistake,
		// and by that time these uses of INDEX have become legitimate
		// CockroachDB features.
		//
		// We are thus left with the need to disambiguate between:
		//
		// CREATE TABLE t(index a) -- column name "index", column type "a"
		// CREATE TABLE t(index (a)) -- keyword INDEX, column name "a"
		// CREATE TABLE t(index a (b)) -- keyword INDEX, index name "a", column name "b"
		//
		// Thankfully, a coldef for a column named "index" and an index
		// specification differ unambiguously, *given sufficient
		// lookaheaed*: an index specification always has an open '('
		// after INDEX, with or without an identifier in-between. A column
		// definition never has this.
		//
		// Likewise, between:
		//
		// ORDER BY index
		// ORDER BY index a@idx
		// ORDER BY index a.b@idx
		// ORDER BY index a.b.c@idx
		//
		// We can unambiguously distinguish by the presence of the '@' sign
		// with a maximum of 6 token lookahead.
		//
		var pprevID, prevID int32
		if l.lastPos > 0 {
			prevID = l.tokens[l.lastPos-1].id
		}
		if l.lastPos > 1 {
			pprevID = l.tokens[l.lastPos-2].id
		}
		var nextID, secondID int32
		if l.lastPos+1 < len(l.tokens) {
			nextID = l.tokens[l.lastPos+1].id
		}
		if l.lastPos+2 < len(l.tokens) {
			secondID = l.tokens[l.lastPos+2].id
		}
		afterCommaOrParen := prevID == ',' || prevID == '('
		afterCommaOrOPTIONS := prevID == ',' || prevID == OPTIONS
		afterCommaOrParenThenINVERTED := prevID == INVERTED && (pprevID == ',' || pprevID == '(')
		followedByParen := nextID == '('
		followedByNonPunctThenParen := nextID > 255 /* non-punctuation */ && secondID == '('
		if //
		// CREATE ... (INDEX (
		// CREATE ... (x INT, y INT, INDEX (
		(afterCommaOrParen && followedByParen) ||
			// SCRUB ... WITH OPTIONS INDEX (...
			// SCRUB ... WITH OPTIONS a, INDEX (...
			(afterCommaOrOPTIONS && followedByParen) ||
			// CREATE ... (INVERTED INDEX (
			// CREATE ... (x INT, y INT, INVERTED INDEX (
			(afterCommaOrParenThenINVERTED && followedByParen) {
			lval.id = INDEX_BEFORE_PAREN
			break
		}
		if //
		// CREATE ... (INDEX abc (
		// CREATE ... (x INT, y INT, INDEX abc (
		(afterCommaOrParen && followedByNonPunctThenParen) ||
			// CREATE ... (INVERTED INDEX abc (
			// CREATE ... (x INT, y INT, INVERTED INDEX abc (
			(afterCommaOrParenThenINVERTED && followedByNonPunctThenParen) {
			lval.id = INDEX_BEFORE_NAME_THEN_PAREN
			break
		}
		// The rules above all require that the INDEX keyword be
		// followed ultimately by an open parenthesis, with no '@'
		// in-between. The rule below is strictly exclusive with this
		// situation.
		afterCommaOrOrderBy := prevID == ',' || (prevID == BY && pprevID == ORDER)
		if afterCommaOrOrderBy {
			// SORT BY INDEX <objname> @
			// SORT BY a, b, INDEX <objname> @
			atSignAfterObjectName := false
			// An object name has one of the following forms:
			//    name
			//    name.name
			//    name.name.name
			// So it is between 1 and 5 tokens in length.
			for i := l.lastPos + 1; i < len(l.tokens) && i < l.lastPos+7; i++ {
				curToken := l.tokens[i].id
				// An object name can only contain keyword/identifiers, and
				// the punctuation '.'.
				if curToken < 255 /* not ident/keyword */ && curToken != '.' && curToken != '@' {
					// Definitely not object name.
					break
				}
				if curToken == '@' {
					if i == l.lastPos+1 {
						/* The '@' cannot follow the INDEX keyword directly. */
						break
					}
					atSignAfterObjectName = true
					break
				}
			}
			if atSignAfterObjectName {
				lval.id = INDEX_AFTER_ORDER_BY_BEFORE_AT
			}
		}

	case NOT, WITH, AS, GENERATED, NULLS, RESET, ROLE, USER, ON, TENANT, CLUSTER, SET:
		nextToken := sqlSymType{}
		if l.lastPos+1 < len(l.tokens) {
			nextToken = l.tokens[l.lastPos+1]
		}
		secondToken := sqlSymType{}
		if l.lastPos+2 < len(l.tokens) {
			secondToken = l.tokens[l.lastPos+2]
		}
		thirdToken := sqlSymType{}
		if l.lastPos+3 < len(l.tokens) {
			thirdToken = l.tokens[l.lastPos+3]
		}

		// If you update these cases, update lex.lookaheadKeywords.
		switch lval.id {
		case AS:
			switch nextToken.id {
			case OF:
				switch secondToken.id {
				case SYSTEM:
					lval.id = AS_LA
				}
			}
		case NOT:
			switch nextToken.id {
			case BETWEEN, IN, LIKE, ILIKE, SIMILAR:
				lval.id = NOT_LA
			}
		case GENERATED:
			switch nextToken.id {
			case ALWAYS:
				lval.id = GENERATED_ALWAYS
			case BY:
				lval.id = GENERATED_BY_DEFAULT
			}

		case WITH:
			switch nextToken.id {
			case TIME, ORDINALITY, BUCKET_COUNT:
				lval.id = WITH_LA
			}
		case NULLS:
			switch nextToken.id {
			case FIRST, LAST:
				lval.id = NULLS_LA
			}
		case RESET:
			switch nextToken.id {
			case ALL:
				lval.id = RESET_ALL
			}
		case ROLE:
			switch nextToken.id {
			case ALL:
				lval.id = ROLE_ALL
			}
		case USER:
			switch nextToken.id {
			case ALL:
				lval.id = USER_ALL
			}
		case ON:
			switch nextToken.id {
			case DELETE:
				lval.id = ON_LA
			case UPDATE:
				switch secondToken.id {
				case NO, RESTRICT, CASCADE, SET:
					lval.id = ON_LA
				}
			}
		case TENANT:
			switch nextToken.id {
			case ALL:
				lval.id = TENANT_ALL
			}
		case CLUSTER:
			switch nextToken.id {
			case ALL:
				lval.id = CLUSTER_ALL
			}
		case SET:
			switch nextToken.id {
			case TRACING:
				// Do not use the lookahead rule for `SET tracing.custom ...`
				if secondToken.str != "." {
					lval.id = SET_TRACING
				}
			case SESSION:
				switch secondToken.id {
				case TRACING:
					// Do not use the lookahead rule for `SET SESSION tracing.custom ...`
					if thirdToken.str != "." {
						lval.id = SET_TRACING
					}
				}
			}
		}
	}

	return int(lval.id)
}

func (l *lexer) lastToken() sqlSymType {
	if l.lastPos < 0 {
		return sqlSymType{}
	}

	if l.lastPos >= len(l.tokens) {
		return sqlSymType{
			id:  0,
			pos: int32(len(l.in)),
			str: "EOF",
		}
	}
	return l.tokens[l.lastPos]
}

// NewAnnotation returns a new annotation index.
func (l *lexer) NewAnnotation() tree.AnnotationIdx {
	l.numAnnotations++
	return l.numAnnotations
}

// SetStmt is called from the parser when the statement is constructed.
func (l *lexer) SetStmt(stmt tree.Statement) {
	l.stmt = stmt
}

// UpdateNumPlaceholders is called from the parser when a placeholder is constructed.
func (l *lexer) UpdateNumPlaceholders(p *tree.Placeholder) {
	if n := int(p.Idx) + 1; l.numPlaceholders < n {
		l.numPlaceholders = n
	}
}

// PurposelyUnimplemented wraps Error, setting lastUnimplementedError.
func (l *lexer) PurposelyUnimplemented(feature string, reason string) {
	// We purposely do not use unimp here, as it appends hints to suggest that
	// the error may be actively tracked as a bug.
	l.lastError = errors.WithHint(
		errors.WithTelemetry(
			pgerror.Newf(pgcode.Syntax, "unimplemented: this syntax"),
			fmt.Sprintf("sql.purposely_unimplemented.%s", feature),
		),
		reason,
	)
	l.populateErrorDetails()
	l.lastError = &tree.UnsupportedError{
		Err:         l.lastError,
		FeatureName: feature,
	}
}

// UnimplementedWithIssue wraps Error, setting lastUnimplementedError.
func (l *lexer) UnimplementedWithIssue(issue int) {
	l.lastError = unimp.NewWithIssue(issue, "this syntax")
	l.populateErrorDetails()
	l.lastError = &tree.UnsupportedError{
		Err:         l.lastError,
		FeatureName: fmt.Sprintf("https://github.com/cockroachdb/cockroach/issues/%d", issue),
	}
}

// UnimplementedWithIssueDetail wraps Error, setting lastUnimplementedError.
func (l *lexer) UnimplementedWithIssueDetail(issue int, detail string) {
	l.lastError = unimp.NewWithIssueDetail(issue, detail, "this syntax")
	l.populateErrorDetails()
	l.lastError = &tree.UnsupportedError{
		Err:         l.lastError,
		FeatureName: detail,
	}
}

// Unimplemented wraps Error, setting lastUnimplementedError.
func (l *lexer) Unimplemented(feature string) {
	l.lastError = unimp.New(feature, "this syntax")
	l.populateErrorDetails()
	l.lastError = &tree.UnsupportedError{
		Err:         l.lastError,
		FeatureName: feature,
	}
}

// setErr is called from parsing action rules to register an error observed
// while running the action. That error becomes the actual "cause" of the
// syntax error.
func (l *lexer) setErr(err error) {
	err = pgerror.WithCandidateCode(err, pgcode.Syntax)
	l.lastError = err
	l.populateErrorDetails()
}

// setErrNoDetails is similar to setErr, but is used for an error that should
// not be further annotated with details.
func (l *lexer) setErrNoDetails(err error) {
	err = pgerror.WithCandidateCode(err, pgcode.Syntax)
	l.lastError = err
}

func (l *lexer) Error(e string) {
	e = strings.TrimPrefix(e, "syntax error: ") // we'll add it again below.
	l.lastError = pgerror.WithCandidateCode(errors.Newf("%s", e), pgcode.Syntax)
	l.populateErrorDetails()
}

// PopulateErrorDetails properly wraps the "last error" field in the lexer.
func PopulateErrorDetails(
	tokID int32, lastTokStr string, lastTokPos int32, lastErr error, lIn string,
) error {
	var retErr error

	if tokID == ERROR {
		// This is a tokenizer (lexical) error: the scanner
		// will have stored the error message in the string field.
		err := pgerror.WithCandidateCode(errors.Newf("lexical error: %s", lastTokStr), pgcode.Syntax)
		retErr = errors.WithSecondaryError(err, lastErr)
	} else {
		// This is a contextual error. Print the provided error message
		// and the error context.
		if !strings.Contains(lastErr.Error(), "syntax error") {
			// "syntax error" is already prepended when the yacc-generated
			// parser encounters a parsing error.
			lastErr = errors.Wrap(lastErr, "syntax error")
		}
		retErr = errors.Wrapf(lastErr, "at or near \"%s\"", lastTokStr)
	}

	// Find the end of the line containing the last token.
	i := strings.IndexByte(lIn[lastTokPos:], '\n')
	if i == -1 {
		i = len(lIn)
	} else {
		i += int(lastTokPos)
	}
	// Find the beginning of the line containing the last token. Note that
	// LastIndexByte returns -1 if '\n' could not be found.
	j := strings.LastIndexByte(lIn[:lastTokPos], '\n') + 1
	// Output everything up to and including the line containing the last token.
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "source SQL:\n%s\n", lIn[:i])
	// Output a caret indicating where the last token starts.
	fmt.Fprintf(&buf, "%s^", strings.Repeat(" ", int(lastTokPos)-j))
	return errors.WithDetail(retErr, buf.String())
}

func (l *lexer) populateErrorDetails() {
	lastTok := l.lastToken()
	l.lastError = PopulateErrorDetails(lastTok.id, lastTok.str, lastTok.pos, l.lastError, l.in)
}

// SetHelp marks the "last error" field in the lexer to become a
// help text. This method is invoked in the error action of the
// parser, so the help text is only produced if the last token
// encountered was HELPTOKEN -- other cases are just syntax errors,
// and in that case we do not want the help text to overwrite the
// lastError field, which was set earlier to contain details about the
// syntax error.
func (l *lexer) SetHelp(msg HelpMessage) {
	if l.lastError == nil {
		l.lastError = pgerror.WithCandidateCode(errors.New("help request"), pgcode.Syntax)
	}

	if lastTok := l.lastToken(); lastTok.id == HELPTOKEN {
		l.populateHelpMsg(msg.String())
	} else {
		if msg.Command != "" {
			l.lastError = errors.WithHintf(l.lastError, `try \h %s`, msg.Command)
		} else {
			l.lastError = errors.WithHintf(l.lastError, `try \hf %s`, msg.Function)
		}
	}
}

// specialHelpErrorPrefix is a special prefix that must be present at
// the start of an error message to be considered a valid help
// response payload by the CLI shell.
const specialHelpErrorPrefix = "help token in input"

func (l *lexer) populateHelpMsg(msg string) {
	l.lastError = errors.WithHint(errors.Wrap(l.lastError, specialHelpErrorPrefix), msg)
}
