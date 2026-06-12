// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of K9s

package model1

import (
	"regexp"
	"sync"
)

const maxRxCacheSize = 128

// regexpCache caches compiled filter expressions. Active filters are
// recompiled on every refresh tick; caching keeps compilation out of the
// hot path.
type regexpCache struct {
	mx sync.Mutex
	cc map[string]*regexp.Regexp
}

var filterRxCache = regexpCache{cc: make(map[string]*regexp.Regexp, 32)}

// CompileFilterRx compiles the given expression, caching the result.
func CompileFilterRx(pattern string) (*regexp.Regexp, error) {
	filterRxCache.mx.Lock()
	defer filterRxCache.mx.Unlock()

	if rx, ok := filterRxCache.cc[pattern]; ok {
		return rx, nil
	}
	rx, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	if len(filterRxCache.cc) >= maxRxCacheSize {
		clear(filterRxCache.cc)
	}
	filterRxCache.cc[pattern] = rx

	return rx, nil
}
