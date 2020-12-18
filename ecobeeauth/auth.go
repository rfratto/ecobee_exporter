// Package ecobeeauth provides a lazily loaded oauth2 token source for the
// Ecobee API using their "pin" authorization flow.
//
// This code is inspired by https://github.com/rspier/go-ecobee/blob/171fa1acecfb8b3a30ad53b33cec8a6bdf0690a9/ecobee/auth.go
// but modified to remove all traces of user interaction.
package ecobeeauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

// This file contains authentication related functions and structs.
var Scopes = []string{"smartRead", "smartWrite"}

type TokenSource struct {
	clientID string

	mut       sync.Mutex
	tok       *oauth2.Token
	cacheFile string
}

// NewTokenSource creates a new TokenSource that can authenticate against the
// ecobee API and cache the resulting token to a file.
//
// If the provided cacheFile does not already contain a token, retrieving the
// Token from the TokenSource will fail. Call GetPin to get a temporary pin
// and authenticate the application in the Ecobee consumer portal. Afterwards,
// call GetToken with the code provided in the GetPin response.
//
// Using cacheFile is optional.
func NewTokenSource(clientID string, cacheFile string) (*TokenSource, error) {
	ts := TokenSource{
		clientID:  clientID,
		cacheFile: cacheFile,
	}
	if cacheFile != "" {
		var tok oauth2.Token
		f, err := os.Open(cacheFile)
		if os.IsNotExist(err) {
			return &ts, nil
		} else if err != nil {
			// Return error back to the client because the problem probably can't be
			// resolved on its own.
			return nil, err
		}
		defer f.Close()

		if err := json.NewDecoder(f).Decode(&tok); err == nil {
			// Only set the token if decoding didn't fail.
			ts.tok = &tok
		}
	}

	return &ts, nil
}

// Token returns the current saved token. To save a token, call SaveToken.
// If no token is saved, an error will be returned.
//
// If the saved token is expired, it will be refreshed and then saved.
func (ts *TokenSource) Token() (*oauth2.Token, error) {
	ts.mut.Lock()
	defer ts.mut.Unlock()

	if ts.tok == nil {
		return nil, fmt.Errorf("token not yet available")
	}

	if !ts.tok.Valid() {
		// Try to refresh the token.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
		defer cancel()
		newTok, err := ts.RefreshToken(ctx, ts.tok)
		if err != nil {
			return nil, fmt.Errorf("could not refresh token: %w", err)
		}

		// Ignore the error here, which would be from caching.
		_ = ts.saveToken(newTok)
	}

	return ts.tok, nil
}

// SaveToken saves and caches the given token.
func (ts *TokenSource) SaveToken(tok *oauth2.Token) error {
	ts.mut.Lock()
	defer ts.mut.Unlock()
	return ts.saveToken(tok)
}

func (ts *TokenSource) saveToken(tok *oauth2.Token) error {
	ts.tok = tok

	if ts.cacheFile != "" {
		f, err := os.OpenFile(ts.cacheFile, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0660)
		if err != nil {
			return fmt.Errorf("failed to cache file: %w", err)
		}
		defer f.Close()

		if err := json.NewEncoder(f).Encode(ts.tok); err != nil {
			return fmt.Errorf("failed to encode token: %w", err)
		}
	}

	return nil
}

// GetPin gets a pin code to use to authenticate.
func (ts *TokenSource) GetPin(ctx context.Context) (*PinResponse, error) {
	uv := url.Values{
		"response_type": {"ecobeePin"},
		"client_id":     {ts.clientID},
		"scope":         {strings.Join(Scopes, ",")},
	}
	u := url.URL{
		Scheme:   "https",
		Host:     "api.ecobee.com",
		Path:     "authorize",
		RawQuery: uv.Encode(),
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("could not create request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error retrieving response: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("invalid server response: %s", resp.Status)
	}

	var pr PinResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return nil, fmt.Errorf("error reading response: %w", err)
	}
	return &pr, nil
}

// GetToken gets a token from a Code in a PinResponse. Will fail if the code
// hasn't been submitted on the Ecobee portal.
//
// To use the token in the TokenSource, call SaveToken.
func (ts *TokenSource) GetToken(ctx context.Context, code string) (*oauth2.Token, error) {
	return ts.getToken(ctx, url.Values{
		"grant_type": {"ecobeePin"},
		"client_id":  {ts.clientID},
		"code":       {code},
	})
}

// RefreshToken will refresh the given token, returning a new token.
//
// To use the refreshed token in the TokenSource, call SaveToken.
func (ts *TokenSource) RefreshToken(ctx context.Context, tok *oauth2.Token) (*oauth2.Token, error) {
	if tok.RefreshToken == "" {
		return nil, fmt.Errorf("token did not have a refresh")
	}
	return ts.getToken(ctx, url.Values{
		"grant_type": {"ecobeePin"},
		"client_id":  {ts.clientID},
		"code":       {tok.RefreshToken},
	})
}

func (ts *TokenSource) getToken(ctx context.Context, uv url.Values) (*oauth2.Token, error) {
	u := url.URL{
		Scheme:   "https",
		Host:     "api.ecobee.com",
		Path:     "token",
		RawQuery: uv.Encode(),
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("could not create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error POSTing request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("invalid server response: %s", resp.Status)
	}

	var t token
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}
	oauthTok := oauth2.Token(t)
	return &oauthTok, nil
}

// token wraps oauth2.Token but implements the extra fields
// used by the ecobee API:
// https://www.ecobee.com/home/developer/api/documentation/v1/auth/pin-api-authorization.shtml
type token oauth2.Token

func (t *token) UnmarshalJSON(b []byte) error {
	type full struct {
		*oauth2.Token
		ExpiresIn int    `json:"expires_in,omitempty"`
		Scope     string `json:"scope,omitempty"`
	}
	var f full
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}

	// Subtract a minute from the expires in to underestimate how much time is
	// left instead of overestimating.
	f.Token.Expiry = time.Now().Add(time.Minute * time.Duration(f.ExpiresIn-1))
	*t = token(*f.Token.WithExtra(map[string]interface{}{
		"scope": f.Scope,
	}))
	return nil
}

// PinResponse is returned by the Ecobee API and holds a pin to enter into
// the website portal and a code to use once the pin has been verified.
type PinResponse struct {
	EcobeePin      string `json:"ecobeePin"`
	Code           string `json:"code"`
	Scope          string `json:"scope"`
	ExpiresMinutes int    `json:"expires_in"`
	Interval       int    `json:"interval"`
}
