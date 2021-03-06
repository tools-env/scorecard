// Copyright 2020 Security Scorecard Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package roundtripper

import (
	"bytes"
	"context"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/oauth2"
)

const GITHUB_AUTH_TOKEN = "GITHUB_AUTH_TOKEN"

// RateLimitRoundTripper is a rate-limit aware http.Transport for Github.
type RateLimitRoundTripper struct {
	Logger         *zap.SugaredLogger
	InnerTransport http.RoundTripper
}

// NewTransport returns a configured http.Transport for use with GitHub
func NewTransport(ctx context.Context, logger *zap.SugaredLogger) http.RoundTripper {
	token := os.Getenv(GITHUB_AUTH_TOKEN)

	// Start with oauth
	transport := http.DefaultTransport
	if token != "" {
		ts := oauth2.StaticTokenSource(
			&oauth2.Token{AccessToken: token},
		)
		transport = oauth2.NewClient(ctx, ts).Transport
	}

	// Wrap that with the rate limiter
	rateLimit := &RateLimitRoundTripper{
		Logger:         logger,
		InnerTransport: transport,
	}

	// Wrap that with the response cacher
	cache := &CachingRoundTripper{
		Logger:         logger,
		innerTransport: rateLimit,
		respCache:      map[url.URL]*http.Response{},
		bodyCache:      map[url.URL][]byte{},
	}

	return cache
}

// Roundtrip handles caching and ratelimiting of responses from GitHub.
func (gh *RateLimitRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := gh.InnerTransport.RoundTrip(r)
	if err != nil {
		return nil, err
	}

	rateLimit := resp.Header.Get("X-RateLimit-Remaining")
	remaining, err := strconv.Atoi(rateLimit)
	if err != nil {
		return resp, nil
	}

	if remaining <= 0 {
		reset, err := strconv.Atoi(resp.Header.Get("X-RateLimit-Reset"))
		if err != nil {
			return resp, nil
		}

		duration := time.Until(time.Unix(int64(reset), 0))
		gh.Logger.Warnf("Rate limit exceeded. Waiting %s to retry...", duration)

		// Retry
		time.Sleep(duration)
		gh.Logger.Warnf("Rate limit exceeded. Retrying...")
		return gh.RoundTrip(r)
	}

	return resp, err
}

type CachingRoundTripper struct {
	innerTransport http.RoundTripper
	respCache      map[url.URL]*http.Response
	bodyCache      map[url.URL][]byte
	mutex          sync.Mutex
	Logger         *zap.SugaredLogger
}

func (rt *CachingRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	// Check the cache
	rt.mutex.Lock()
	defer rt.mutex.Unlock()
	resp, ok := rt.respCache[*r.URL]

	if ok {
		rt.Logger.Debugf("Cache hit on %s", r.URL.String())
		resp.Body = ioutil.NopCloser(bytes.NewReader(rt.bodyCache[*r.URL]))
		return resp, nil
	}

	// Get the real value
	resp, err := rt.innerTransport.RoundTrip(r)
	if err != nil {
		return nil, err
	}

	// Add to cache
	if resp.StatusCode == http.StatusOK {
		defer resp.Body.Close()
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}

		rt.respCache[*r.URL] = resp
		rt.bodyCache[*r.URL] = body

		resp.Body = ioutil.NopCloser(bytes.NewReader(body))
	}
	return resp, err
}
