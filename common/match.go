package common

import "slices"

// SelectExecutor picks the best matching executor from `execs` for the
// given (method, finality) pair using a 4-tier priority:
//
//  1. exact-method + finality match
//  2. exact-method (or wildcard-method) match
//  3. finality-only match (matchMethod="" or "*")
//  4. catch-all (matchMethod="" or "*" AND empty finality list)
//
// The two getter callbacks let the matcher work on any executor type
// without an interface — pass closures over your concrete struct's
// fields.
//
// Returns the zero value of E if no executor matches.
func SelectExecutor[E any](
	execs []E,
	method string,
	finality DataFinalityState,
	getMethod func(E) string,
	getFinality func(E) []DataFinalityState,
) (E, bool) {
	var zero E
	if len(execs) == 0 {
		return zero, false
	}

	var bestMethodFinality, bestMethod, bestFinality, bestCatchAll *E

	for i := range execs {
		e := execs[i]
		mPat := getMethod(e)
		fList := getFinality(e)

		methodMatch := mPat == "" || mPat == "*" || matchMethodPattern(mPat, method)
		finalityMatch := len(fList) == 0 || slices.Contains(fList, finality)

		isWildMethod := mPat == "" || mPat == "*"
		isWildFinality := len(fList) == 0

		if !methodMatch || !finalityMatch {
			continue
		}

		switch {
		case !isWildMethod && !isWildFinality:
			if bestMethodFinality == nil {
				bestMethodFinality = &execs[i]
			}
		case !isWildMethod && isWildFinality:
			if bestMethod == nil {
				bestMethod = &execs[i]
			}
		case isWildMethod && !isWildFinality:
			if bestFinality == nil {
				bestFinality = &execs[i]
			}
		default:
			if bestCatchAll == nil {
				bestCatchAll = &execs[i]
			}
		}
	}

	switch {
	case bestMethodFinality != nil:
		return *bestMethodFinality, true
	case bestMethod != nil:
		return *bestMethod, true
	case bestFinality != nil:
		return *bestFinality, true
	case bestCatchAll != nil:
		return *bestCatchAll, true
	}
	return zero, false
}

// matchMethodPattern returns true when pattern matches method using the
// project's wildcard semantics. Falls back to WildcardMatch from
// matcher.go.
func matchMethodPattern(pattern, method string) bool {
	if pattern == "" || pattern == "*" {
		return true
	}
	if pattern == method {
		return true
	}
	m, _ := WildcardMatch(pattern, method)
	return m
}
