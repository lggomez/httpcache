package httpcache

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

type cacheControl map[string]string

type entryFreshness int

const (
	stale entryFreshness = iota
	fresh
	transparent
)

func (f entryFreshness) String() string {
	switch f {
	case stale:
		return "stale"
	case fresh:
		return "fresh"
	case transparent:
		return "transparent"
	}

	return "undefined"
}

func parseCacheControl(headers http.Header) cacheControl {
	cc := cacheControl{}
	ccHeader := headers.Get("Cache-Control")
	for _, part := range strings.Split(ccHeader, ",") {
		part = strings.Trim(part, " ")
		if part == "" {
			continue
		}
		if strings.ContainsRune(part, '=') {
			keyVal := strings.Split(part, "=")
			cc[strings.Trim(keyVal[0], " ")] = strings.Trim(keyVal[1], ",")
		} else {
			cc[part] = ""
		}
	}
	return cc
}

func canStore(reqCacheControl, respCacheControl cacheControl) (canStore bool) {
	if _, ok := respCacheControl["no-store"]; ok {
		return false
	}
	if _, ok := reqCacheControl["no-store"]; ok {
		return false
	}
	return true
}

// Returns true if either the request or the response includes the stale-if-error
// cache control extension: https://tools.ietf.org/html/rfc5861
func canStaleOnError(respHeaders, reqHeaders http.Header) bool {
	respCacheControl := parseCacheControl(respHeaders)
	reqCacheControl := parseCacheControl(reqHeaders)

	var err error
	lifetime := time.Duration(-1)

	if staleMaxAge, ok := respCacheControl["stale-if-error"]; ok {
		if staleMaxAge != "" {
			lifetime, err = time.ParseDuration(staleMaxAge + "s")
			if err != nil {
				return false
			}
		} else {
			return true
		}
	}
	if staleMaxAge, ok := reqCacheControl["stale-if-error"]; ok {
		if staleMaxAge != "" {
			lifetime, err = time.ParseDuration(staleMaxAge + "s")
			if err != nil {
				return false
			}
		} else {
			return true
		}
	}

	if lifetime >= 0 {
		date, err := Date(respHeaders)
		if err != nil {
			return false
		}
		currentAge := clock.since(date)
		if lifetime > currentAge {
			return true
		}
	}

	return false
}

// getFreshness will return one of fresh/stale/transparent based on the cache-control
// values of the request and the response
//
// fresh indicates the response can be returned
// stale indicates that the response needs validating before it is returned
// transparent indicates the response should not be used to fulfil the request
//
// Because this is only a private cache, 'public' and 'private' in cache-control aren't
// significant. Similarly, smax-age isn't used.
func (cc *CachedClient) getFreshness(req *http.Request, respHeaders http.Header) (freshness entryFreshness) {
	reqHeaders := req.Header
	respCacheControl := parseCacheControl(respHeaders)
	reqCacheControl := parseCacheControl(reqHeaders)
	if _, ok := reqCacheControl["no-cache"]; ok {
		cc.log(fmt.Sprintf("[httpcache](%p) request no-cache header found. returning transparent freshness", req))
		return transparent
	}
	if _, ok := respCacheControl["no-cache"]; ok {
		cc.log(fmt.Sprintf("[httpcache](%p) response no-cache header found. returning stale freshness", req))
		return stale
	}
	if _, ok := reqCacheControl["only-if-cached"]; ok {
		cc.log(fmt.Sprintf("[httpcache](%p) request only-if-cached header found. returning fresh freshness", req))
		return fresh
	}

	date, err := Date(respHeaders)
	if err != nil {
		cc.log(fmt.Sprintf("[httpcache](%p) response date get error. returning stale freshness (%v)", req, err.Error()))
		return stale
	}
	currentAge := clock.since(date)

	var lifetime time.Duration
	var zeroDuration time.Duration

	// If a response includes both an Expires header and a max-age directive,
	// the max-age directive overrides the Expires header, even if the Expires header is more restrictive.
	if maxAge, ok := respCacheControl["max-age"]; ok {
		lifetime, err = time.ParseDuration(maxAge + "s")
		if err != nil {
			lifetime = zeroDuration
		}
	} else {
		expiresHeader := respHeaders.Get("Expires")
		if expiresHeader != "" {
			expires, err := time.Parse(time.RFC1123, expiresHeader)
			if err != nil {
				lifetime = zeroDuration
			} else {
				lifetime = expires.Sub(date)
			}
		}
	}

	if maxAge, ok := reqCacheControl["max-age"]; ok {
		// the client is willing to accept a response whose age is no greater than the specified time in seconds
		lifetime, err = time.ParseDuration(maxAge + "s")
		if err != nil {
			lifetime = zeroDuration
		}
	}
	if minfresh, ok := reqCacheControl["min-fresh"]; ok {
		//  the client wants a response that will still be fresh for at least the specified number of seconds.
		minfreshDuration, err := time.ParseDuration(minfresh + "s")
		if err == nil {
			currentAge = time.Duration(currentAge + minfreshDuration)
		}
	}

	if maxstale, ok := reqCacheControl["max-stale"]; ok {
		// Indicates that the client is willing to accept a response that has exceeded its expiration time.
		// If max-stale is assigned a value, then the client is willing to accept a response that has exceeded
		// its expiration time by no more than the specified number of seconds.
		// If no value is assigned to max-stale, then the client is willing to accept a stale response of any age.
		//
		// Responses served only because of a max-stale value are supposed to have a Warning header added to them,
		// but that seems like a  hassle, and is it actually useful? If so, then there needs to be a different
		// return-value available here.
		if maxstale == "" {
			cc.log(fmt.Sprintf("[httpcache](%p) request max-stale header found. returning fresh freshness", req))
			return fresh
		}
		maxstaleDuration, err := time.ParseDuration(maxstale + "s")
		if err == nil {
			currentAge = time.Duration(currentAge - maxstaleDuration)
		}
	}

	if lifetime > currentAge {
		cc.log(fmt.Sprintf("[httpcache](%p) lifetime > currentAge. returning fresh freshness (%s, %s)", req, lifetime, currentAge))
		return fresh
	}

	cc.log(fmt.Sprintf("[httpcache](%p) cannot infer freshness. fallback to stale freshness (lifetime: %s <= currentAge: %s)", req, lifetime, currentAge))
	return stale
}
