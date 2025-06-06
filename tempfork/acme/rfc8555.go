// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package acme

import (
	"context"
	"crypto"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DeactivateReg permanently disables an existing account associated with c.Key.
// A deactivated account can no longer request certificate issuance or access
// resources related to the account, such as orders or authorizations.
//
// It only works with CAs implementing RFC 8555.
func (c *Client) DeactivateReg(ctx context.Context) error {
	if _, err := c.Discover(ctx); err != nil { // required by c.accountKID
		return err
	}
	url := string(c.accountKID(ctx))
	if url == "" {
		return ErrNoAccount
	}
	req := json.RawMessage(`{"status": "deactivated"}`)
	res, err := c.post(ctx, nil, url, req, wantStatus(http.StatusOK))
	if err != nil {
		return err
	}
	res.Body.Close()
	return nil
}

// registerRFC is equivalent to c.Register but for CAs implementing RFC 8555.
// It expects c.Discover to have already been called.
func (c *Client) registerRFC(ctx context.Context, acct *Account, prompt func(tosURL string) bool) (*Account, error) {
	c.cacheMu.Lock() // guard c.kid access
	defer c.cacheMu.Unlock()

	req := struct {
		TermsAgreed            bool              `json:"termsOfServiceAgreed,omitempty"`
		Contact                []string          `json:"contact,omitempty"`
		ExternalAccountBinding *jsonWebSignature `json:"externalAccountBinding,omitempty"`
	}{
		Contact: acct.Contact,
	}
	if c.dir.Terms != "" {
		req.TermsAgreed = prompt(c.dir.Terms)
	}

	// set 'externalAccountBinding' field if requested
	if acct.ExternalAccountBinding != nil {
		eabJWS, err := c.encodeExternalAccountBinding(acct.ExternalAccountBinding)
		if err != nil {
			return nil, fmt.Errorf("acme: failed to encode external account binding: %v", err)
		}
		req.ExternalAccountBinding = eabJWS
	}

	res, err := c.post(ctx, c.Key, c.dir.RegURL, req, wantStatus(
		http.StatusOK,      // account with this key already registered
		http.StatusCreated, // new account created
	))
	if err != nil {
		return nil, err
	}

	defer res.Body.Close()
	a, err := responseAccount(res)
	if err != nil {
		return nil, err
	}
	// Cache Account URL even if we return an error to the caller.
	// It is by all means a valid and usable "kid" value for future requests.
	c.KID = KeyID(a.URI)
	if res.StatusCode == http.StatusOK {
		return nil, ErrAccountAlreadyExists
	}
	return a, nil
}

// encodeExternalAccountBinding will encode an external account binding stanza
// as described in https://tools.ietf.org/html/rfc8555#section-7.3.4.
func (c *Client) encodeExternalAccountBinding(eab *ExternalAccountBinding) (*jsonWebSignature, error) {
	jwk, err := jwkEncode(c.Key.Public())
	if err != nil {
		return nil, err
	}
	return jwsWithMAC(eab.Key, eab.KID, c.dir.RegURL, []byte(jwk))
}

// updateRegRFC is equivalent to c.UpdateReg but for CAs implementing RFC 8555.
// It expects c.Discover to have already been called.
func (c *Client) updateRegRFC(ctx context.Context, a *Account) (*Account, error) {
	url := string(c.accountKID(ctx))
	if url == "" {
		return nil, ErrNoAccount
	}
	req := struct {
		Contact []string `json:"contact,omitempty"`
	}{
		Contact: a.Contact,
	}
	res, err := c.post(ctx, nil, url, req, wantStatus(http.StatusOK))
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	return responseAccount(res)
}

// getRegRFC is equivalent to c.GetReg but for CAs implementing RFC 8555.
// It expects c.Discover to have already been called.
func (c *Client) getRegRFC(ctx context.Context) (*Account, error) {
	req := json.RawMessage(`{"onlyReturnExisting": true}`)
	res, err := c.post(ctx, c.Key, c.dir.RegURL, req, wantStatus(http.StatusOK))
	if e, ok := err.(*Error); ok && e.ProblemType == "urn:ietf:params:acme:error:accountDoesNotExist" {
		return nil, ErrNoAccount
	}
	if err != nil {
		return nil, err
	}

	defer res.Body.Close()
	return responseAccount(res)
}

func responseAccount(res *http.Response) (*Account, error) {
	var v struct {
		Status  string
		Contact []string
		Orders  string
	}
	if err := json.NewDecoder(res.Body).Decode(&v); err != nil {
		return nil, fmt.Errorf("acme: invalid account response: %v", err)
	}
	return &Account{
		URI:       res.Header.Get("Location"),
		Status:    v.Status,
		Contact:   v.Contact,
		OrdersURL: v.Orders,
	}, nil
}

// accountKeyRollover attempts to perform account key rollover.
// On success it will change client.Key to the new key.
func (c *Client) accountKeyRollover(ctx context.Context, newKey crypto.Signer) error {
	dir, err := c.Discover(ctx) // Also required by c.accountKID
	if err != nil {
		return err
	}
	kid := c.accountKID(ctx)
	if kid == noKeyID {
		return ErrNoAccount
	}
	oldKey, err := jwkEncode(c.Key.Public())
	if err != nil {
		return err
	}
	payload := struct {
		Account string          `json:"account"`
		OldKey  json.RawMessage `json:"oldKey"`
	}{
		Account: string(kid),
		OldKey:  json.RawMessage(oldKey),
	}
	inner, err := jwsEncodeJSON(payload, newKey, noKeyID, noNonce, dir.KeyChangeURL)
	if err != nil {
		return err
	}

	res, err := c.post(ctx, nil, dir.KeyChangeURL, base64.RawURLEncoding.EncodeToString(inner), wantStatus(http.StatusOK))
	if err != nil {
		return err
	}
	defer res.Body.Close()
	c.Key = newKey
	return nil
}

// AuthorizeOrder initiates the order-based application for certificate issuance,
// as opposed to pre-authorization in Authorize.
// It is only supported by CAs implementing RFC 8555.
//
// The caller then needs to fetch each authorization with GetAuthorization,
// identify those with StatusPending status and fulfill a challenge using Accept.
// Once all authorizations are satisfied, the caller will typically want to poll
// order status using WaitOrder until it's in StatusReady state.
// To finalize the order and obtain a certificate, the caller submits a CSR with CreateOrderCert.
func (c *Client) AuthorizeOrder(ctx context.Context, id []AuthzID, opt ...OrderOption) (*Order, error) {
	dir, err := c.Discover(ctx)
	if err != nil {
		return nil, err
	}

	req := struct {
		Identifiers []wireAuthzID `json:"identifiers"`
		NotBefore   string        `json:"notBefore,omitempty"`
		NotAfter    string        `json:"notAfter,omitempty"`
		Replaces    string        `json:"replaces,omitempty"`
	}{}
	for _, v := range id {
		req.Identifiers = append(req.Identifiers, wireAuthzID{
			Type:  v.Type,
			Value: v.Value,
		})
	}
	for _, o := range opt {
		switch o := o.(type) {
		case orderNotBeforeOpt:
			req.NotBefore = time.Time(o).Format(time.RFC3339)
		case orderNotAfterOpt:
			req.NotAfter = time.Time(o).Format(time.RFC3339)
		case orderReplacesCert:
			req.Replaces = certRenewalIdentifier(o.cert)
		case orderReplacesCertDER:
			cert, err := x509.ParseCertificate(o)
			if err != nil {
				return nil, fmt.Errorf("failed to parse certificate being replaced: %w", err)
			}
			req.Replaces = certRenewalIdentifier(cert)
		default:
			// Package's fault if we let this happen.
			panic(fmt.Sprintf("unsupported order option type %T", o))
		}
	}

	res, err := c.post(ctx, nil, dir.OrderURL, req, wantStatus(http.StatusCreated))
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	return responseOrder(res)
}

// GetOrder retrives an order identified by the given URL.
// For orders created with AuthorizeOrder, the url value is Order.URI.
//
// If a caller needs to poll an order until its status is final,
// see the WaitOrder method.
func (c *Client) GetOrder(ctx context.Context, url string) (*Order, error) {
	if _, err := c.Discover(ctx); err != nil {
		return nil, err
	}

	res, err := c.postAsGet(ctx, url, wantStatus(http.StatusOK))
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	return responseOrder(res)
}

// WaitOrder polls an order from the given URL until it is in one of the final states,
// StatusReady, StatusValid or StatusInvalid, the CA responded with a non-retryable error
// or the context is done.
//
// It returns a non-nil Order only if its Status is StatusReady or StatusValid.
// In all other cases WaitOrder returns an error.
// If the Status is StatusInvalid, the returned error is of type *OrderError.
func (c *Client) WaitOrder(ctx context.Context, url string) (*Order, error) {
	if _, err := c.Discover(ctx); err != nil {
		return nil, err
	}
	for {
		res, err := c.postAsGet(ctx, url, wantStatus(http.StatusOK))
		if err != nil {
			return nil, err
		}
		o, err := responseOrder(res)
		res.Body.Close()
		switch {
		case err != nil:
			// Skip and retry.
		case o.Status == StatusInvalid:
			return nil, &OrderError{OrderURL: o.URI, Status: o.Status}
		case o.Status == StatusReady || o.Status == StatusValid:
			return o, nil
		}

		d := retryAfter(res.Header.Get("Retry-After"))
		if d == 0 {
			// Default retry-after.
			// Same reasoning as in WaitAuthorization.
			d = time.Second
		}
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
			// Retry.
		}
	}
}

func responseOrder(res *http.Response) (*Order, error) {
	var v struct {
		Status         string
		Expires        time.Time
		Identifiers    []wireAuthzID
		NotBefore      time.Time
		NotAfter       time.Time
		Error          *wireError
		Authorizations []string
		Finalize       string
		Certificate    string
	}
	if err := json.NewDecoder(res.Body).Decode(&v); err != nil {
		return nil, fmt.Errorf("acme: error reading order: %v", err)
	}
	o := &Order{
		URI:         res.Header.Get("Location"),
		Status:      v.Status,
		Expires:     v.Expires,
		NotBefore:   v.NotBefore,
		NotAfter:    v.NotAfter,
		AuthzURLs:   v.Authorizations,
		FinalizeURL: v.Finalize,
		CertURL:     v.Certificate,
	}
	for _, id := range v.Identifiers {
		o.Identifiers = append(o.Identifiers, AuthzID{Type: id.Type, Value: id.Value})
	}
	if v.Error != nil {
		o.Error = v.Error.error(nil /* headers */)
	}
	return o, nil
}

// CreateOrderCert submits the CSR (Certificate Signing Request) to a CA at the specified URL.
// The URL is the FinalizeURL field of an Order created with AuthorizeOrder.
//
// If the bundle argument is true, the returned value also contain the CA (issuer)
// certificate chain. Otherwise, only a leaf certificate is returned.
// The returned URL can be used to re-fetch the certificate using FetchCert.
//
// This method is only supported by CAs implementing RFC 8555. See CreateCert for pre-RFC CAs.
//
// CreateOrderCert returns an error if the CA's response is unreasonably large.
// Callers are encouraged to parse the returned value to ensure the certificate is valid and has the expected features.
func (c *Client) CreateOrderCert(ctx context.Context, url string, csr []byte, bundle bool) (der [][]byte, certURL string, err error) {
	if _, err := c.Discover(ctx); err != nil { // required by c.accountKID
		return nil, "", err
	}

	// RFC describes this as "finalize order" request.
	req := struct {
		CSR string `json:"csr"`
	}{
		CSR: base64.RawURLEncoding.EncodeToString(csr),
	}
	res, err := c.post(ctx, nil, url, req, wantStatus(http.StatusOK))
	if err != nil {
		return nil, "", err
	}
	defer res.Body.Close()
	o, err := responseOrder(res)
	if err != nil {
		return nil, "", err
	}

	// Wait for CA to issue the cert if they haven't.
	if o.Status != StatusValid {
		o, err = c.WaitOrder(ctx, o.URI)
	}
	if err != nil {
		return nil, "", err
	}
	// The only acceptable status post finalize and WaitOrder is "valid".
	if o.Status != StatusValid {
		return nil, "", &OrderError{OrderURL: o.URI, Status: o.Status}
	}
	crt, err := c.fetchCertRFC(ctx, o.CertURL, bundle)
	return crt, o.CertURL, err
}

// fetchCertRFC downloads issued certificate from the given URL.
// It expects the CA to respond with PEM-encoded certificate chain.
//
// The URL argument is the CertURL field of Order.
func (c *Client) fetchCertRFC(ctx context.Context, url string, bundle bool) ([][]byte, error) {
	res, err := c.postAsGet(ctx, url, wantStatus(http.StatusOK))
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	// Get all the bytes up to a sane maximum.
	// Account very roughly for base64 overhead.
	const max = maxCertChainSize + maxCertChainSize/33
	b, err := io.ReadAll(io.LimitReader(res.Body, max+1))
	if err != nil {
		return nil, fmt.Errorf("acme: fetch cert response stream: %v", err)
	}
	if len(b) > max {
		return nil, errors.New("acme: certificate chain is too big")
	}

	// Decode PEM chain.
	var chain [][]byte
	for {
		var p *pem.Block
		p, b = pem.Decode(b)
		if p == nil {
			break
		}
		if p.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("acme: invalid PEM cert type %q", p.Type)
		}

		chain = append(chain, p.Bytes)
		if !bundle {
			return chain, nil
		}
		if len(chain) > maxChainLen {
			return nil, errors.New("acme: certificate chain is too long")
		}
	}
	if len(chain) == 0 {
		return nil, errors.New("acme: certificate chain is empty")
	}
	return chain, nil
}

// sends a cert revocation request in either JWK form when key is non-nil or KID form otherwise.
func (c *Client) revokeCertRFC(ctx context.Context, key crypto.Signer, cert []byte, reason CRLReasonCode) error {
	req := &struct {
		Cert   string `json:"certificate"`
		Reason int    `json:"reason"`
	}{
		Cert:   base64.RawURLEncoding.EncodeToString(cert),
		Reason: int(reason),
	}
	res, err := c.post(ctx, key, c.dir.RevokeURL, req, wantStatus(http.StatusOK))
	if err != nil {
		if isAlreadyRevoked(err) {
			// Assume it is not an error to revoke an already revoked cert.
			return nil
		}
		return err
	}
	defer res.Body.Close()
	return nil
}

func isAlreadyRevoked(err error) bool {
	e, ok := err.(*Error)
	return ok && e.ProblemType == "urn:ietf:params:acme:error:alreadyRevoked"
}

// ListCertAlternates retrieves any alternate certificate chain URLs for the
// given certificate chain URL. These alternate URLs can be passed to FetchCert
// in order to retrieve the alternate certificate chains.
//
// If there are no alternate issuer certificate chains, a nil slice will be
// returned.
func (c *Client) ListCertAlternates(ctx context.Context, url string) ([]string, error) {
	if _, err := c.Discover(ctx); err != nil { // required by c.accountKID
		return nil, err
	}

	res, err := c.postAsGet(ctx, url, wantStatus(http.StatusOK))
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	// We don't need the body but we need to discard it so we don't end up
	// preventing keep-alive
	if _, err := io.Copy(io.Discard, res.Body); err != nil {
		return nil, fmt.Errorf("acme: cert alternates response stream: %v", err)
	}
	alts := linkHeader(res.Header, "alternate")
	return alts, nil
}
