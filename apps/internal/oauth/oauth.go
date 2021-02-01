// Copyright (c) Microsoft Corporation.
// Licensed under the MIT license.

package oauth

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/AzureAD/microsoft-authentication-library-for-go/apps/internal/oauth/ops"
	"github.com/AzureAD/microsoft-authentication-library-for-go/apps/internal/oauth/ops/accesstokens"
	"github.com/AzureAD/microsoft-authentication-library-for-go/apps/internal/oauth/ops/authority"
	"github.com/AzureAD/microsoft-authentication-library-for-go/apps/internal/oauth/ops/wstrust"
	"github.com/AzureAD/microsoft-authentication-library-for-go/apps/internal/oauth/ops/wstrust/defs"
)

type resolveEndpointer interface {
	ResolveEndpoints(ctx context.Context, authorityInfo authority.Info, userPrincipalName string) (authority.Endpoints, error)
}

type accessTokens interface {
	DeviceCodeResult(ctx context.Context, authParameters authority.AuthParams) (accesstokens.DeviceCodeResult, error)
	FromUsernamePassword(ctx context.Context, authParameters authority.AuthParams) (accesstokens.TokenResponse, error)
	FromAuthCode(ctx context.Context, req accesstokens.AuthCodeRequest) (accesstokens.TokenResponse, error)
	FromRefreshToken(ctx context.Context, appType accesstokens.AppType, authParams authority.AuthParams, cc *accesstokens.Credential, refreshToken string) (accesstokens.TokenResponse, error)
	FromClientSecret(ctx context.Context, authParameters authority.AuthParams, clientSecret string) (accesstokens.TokenResponse, error)
	FromAssertion(ctx context.Context, authParameters authority.AuthParams, assertion string) (accesstokens.TokenResponse, error)
	FromDeviceCodeResult(ctx context.Context, authParameters authority.AuthParams, deviceCodeResult accesstokens.DeviceCodeResult) (accesstokens.TokenResponse, error)
	FromSamlGrant(ctx context.Context, authParameters authority.AuthParams, samlGrant wstrust.SamlTokenInfo) (accesstokens.TokenResponse, error)
}

// fetchAuthority will be implemented by authority.Authority.
type fetchAuthority interface {
	UserRealm(context.Context, authority.AuthParams) (authority.UserRealm, error)
	AADInstanceDiscovery(context.Context, authority.Info) (authority.InstanceDiscoveryResponse, error)
}

type fetchWSTrust interface {
	Mex(ctx context.Context, federationMetadataURL string) (defs.MexDocument, error)
	SAMLTokenInfo(ctx context.Context, authParameters authority.AuthParams, cloudAudienceURN string, endpoint defs.Endpoint) (wstrust.SamlTokenInfo, error)
}

// Client provides tokens for various types of token requests.
type Client struct {
	resolver     resolveEndpointer
	accessTokens accessTokens
	authority    fetchAuthority
	wsTrust      fetchWSTrust
}

// New is the constructor for Token.
func New(httpClient ops.HTTPClient) *Client {
	r := ops.New(httpClient)
	return &Client{
		resolver:     newAuthorityEndpoint(r),
		accessTokens: r.AccessTokens(),
		authority:    r.Authority(),
		wsTrust:      r.WSTrust(),
	}
}

// ResolveEndpoints gets the authorization and token endpoints and creates an AuthorityEndpoints instance.
func (t *Client) ResolveEndpoints(ctx context.Context, authorityInfo authority.Info, userPrincipalName string) (authority.Endpoints, error) {
	return t.resolver.ResolveEndpoints(ctx, authorityInfo, userPrincipalName)
}

func (t *Client) AADInstanceDiscovery(ctx context.Context, authorityInfo authority.Info) (authority.InstanceDiscoveryResponse, error) {
	return t.authority.AADInstanceDiscovery(ctx, authorityInfo)
}

// AuthCode returns a token based on an authorization code.
func (t *Client) AuthCode(ctx context.Context, req accesstokens.AuthCodeRequest) (accesstokens.TokenResponse, error) {
	if err := t.resolveEndpoint(ctx, &req.AuthParams, ""); err != nil {
		return accesstokens.TokenResponse{}, err
	}

	tResp, err := t.accessTokens.FromAuthCode(ctx, req)
	if err != nil {
		return accesstokens.TokenResponse{}, fmt.Errorf("could not retrieve token from auth code: %w", err)
	}
	return tResp, nil
}

// Credential acquires a token from the authority using a client credentials grant.
func (t *Client) Credential(ctx context.Context, authParams authority.AuthParams, cred *accesstokens.Credential) (accesstokens.TokenResponse, error) {
	if err := t.resolveEndpoint(ctx, &authParams, ""); err != nil {
		return accesstokens.TokenResponse{}, err
	}

	if cred.Secret != "" {
		return t.accessTokens.FromClientSecret(ctx, authParams, cred.Secret)
	}

	jwt, err := cred.JWT(authParams)
	if err != nil {
		return accesstokens.TokenResponse{}, err
	}
	return t.accessTokens.FromAssertion(ctx, authParams, jwt)
}

func (t *Client) Refresh(ctx context.Context, reqType accesstokens.AppType, authParams authority.AuthParams, cc *accesstokens.Credential, refreshToken accesstokens.RefreshToken) (accesstokens.TokenResponse, error) {
	if err := t.resolveEndpoint(ctx, &authParams, ""); err != nil {
		return accesstokens.TokenResponse{}, err
	}

	return t.accessTokens.FromRefreshToken(ctx, reqType, authParams, cc, refreshToken.Secret)
}

// UsernamePassword retrieves a token where a username and password is used. However, if this is
// a user realm of "Federated", this uses SAML tokens. If "Managed", uses normal username/password.
func (t *Client) UsernamePassword(ctx context.Context, authParams authority.AuthParams) (accesstokens.TokenResponse, error) {
	if err := t.resolveEndpoint(ctx, &authParams, ""); err != nil {
		return accesstokens.TokenResponse{}, err
	}

	userRealm, err := t.authority.UserRealm(ctx, authParams)
	if err != nil {
		return accesstokens.TokenResponse{}, fmt.Errorf("problem getting user realm(user: %s) from authority: %w", authParams.Username, err)
	}

	switch userRealm.AccountType {
	case authority.Federated:
		mexDoc, err := t.wsTrust.Mex(ctx, userRealm.FederationMetadataURL)
		if err != nil {
			return accesstokens.TokenResponse{}, fmt.Errorf("problem getting mex doc from federated url(%s): %w", userRealm.FederationMetadataURL, err)
		}

		saml, err := t.wsTrust.SAMLTokenInfo(ctx, authParams, userRealm.CloudAudienceURN, mexDoc.UsernamePasswordEndpoint)
		if err != nil {
			return accesstokens.TokenResponse{}, fmt.Errorf("problem getting SAML token info: %w", err)
		}
		return t.accessTokens.FromSamlGrant(ctx, authParams, saml)
	case authority.Managed:
		return t.accessTokens.FromUsernamePassword(ctx, authParams)
	}
	return accesstokens.TokenResponse{}, errors.New("unknown account type")
}

// DeviceCode is the result of a call to Token.DeviceCode().
type DeviceCode struct {
	// Result is the device code result from the first call in the device code flow. This allows
	// the caller to retrieve the displayed code that is used to authorize on the second device.
	Result     accesstokens.DeviceCodeResult
	authParams authority.AuthParams

	accessTokens accessTokens
}

// Token returns a token AFTER the user uses the device code on the second device. This will block
// until either: (1) the code is input by the user and the service releases a token, (2) the token
// expires, (3) the Context passed to .DeviceCode() is cancelled or expires, (4) some other service
// error occurs.
func (d DeviceCode) Token(ctx context.Context) (accesstokens.TokenResponse, error) {
	if d.accessTokens == nil {
		return accesstokens.TokenResponse{}, fmt.Errorf("DeviceCode was either created outside its package or the creating method had an error. DeviceCode is not valid")
	}

	var cancel context.CancelFunc
	d.Result.ExpiresOn.Sub(time.Now().UTC())
	if deadline, ok := ctx.Deadline(); !ok || d.Result.ExpiresOn.Before(deadline) {
		ctx, cancel = context.WithDeadline(ctx, d.Result.ExpiresOn)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	var interval = 50 * time.Millisecond
	timer := time.NewTimer(interval)
	defer timer.Stop()

	for {
		timer.Reset(interval)
		select {
		case <-ctx.Done():
			return accesstokens.TokenResponse{}, ctx.Err()
		case <-timer.C:
			interval += interval * 2
			if interval > 5*time.Second {
				interval = 5 * time.Second
			}
		}

		token, err := d.accessTokens.FromDeviceCodeResult(ctx, d.authParams, d.Result)
		if err != nil && isWaitDeviceCodeErr(err) {
			continue
		}
		return token, err // This handles if it was a non-wait error or success
	}
}

var waitRE = regexp.MustCompile("(authorization_pending|slow_down)")

// TODO(msal): This is freaking terrible. The original just looked for the exact word in the error output.
// I doubt this worked. I don't know if the service really does this, but it should send back a structured
// error response. Anyways, I updated this to search the entire return error message, which will be the body
// of the return.
func isWaitDeviceCodeErr(err error) bool {
	return waitRE.MatchString(err.Error())
}

// DeviceCode returns a DeviceCode object that can be used to get the code that must be entered on the second
// device and optionally the token once the code has been entered on the second device.
func (t *Client) DeviceCode(ctx context.Context, authParams authority.AuthParams) (DeviceCode, error) {
	if err := t.resolveEndpoint(ctx, &authParams, ""); err != nil {
		return DeviceCode{}, err
	}

	dcr, err := t.accessTokens.DeviceCodeResult(ctx, authParams)
	if err != nil {
		return DeviceCode{}, err
	}

	return DeviceCode{Result: dcr, authParams: authParams, accessTokens: t.accessTokens}, nil
}

func (t *Client) resolveEndpoint(ctx context.Context, authParams *authority.AuthParams, userPrincipalName string) error {
	endpoints, err := t.resolver.ResolveEndpoints(ctx, authParams.AuthorityInfo, userPrincipalName)
	if err != nil {
		return fmt.Errorf("unable to resolve an endpoint: %s", err)
	}
	authParams.Endpoints = endpoints
	return nil
}