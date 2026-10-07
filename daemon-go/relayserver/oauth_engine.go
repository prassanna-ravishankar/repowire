package relayserver

import (
	"context"
	"time"

	"github.com/go-oauth2/oauth2/v4"
	"github.com/go-oauth2/oauth2/v4/errors"
	"github.com/go-oauth2/oauth2/v4/manage"
	"github.com/go-oauth2/oauth2/v4/models"
	"github.com/go-oauth2/oauth2/v4/server"
)

// oauthEngine adapts the library to one SQLite transaction. Library operations
// (including consume+mint and refresh rotation) commit together, never partially.
func oauthEngine(d *oauthData) (*manage.Manager, *server.Server) {
	adapter := &oauthAdapter{d}
	manager := manage.NewDefaultManager()
	manager.MapClientStorage(adapter)
	manager.MapTokenStorage(adapter)
	manager.MapAccessGenerate(opaqueGenerate{})
	manager.MapAuthorizeGenerate(codeGenerate{})
	manager.SetAuthorizeCodeExp(time.Minute)
	manager.SetAuthorizeCodeTokenCfg(&manage.Config{AccessTokenExp: accessLifetime, RefreshTokenExp: grantLifetime, IsGenerateRefresh: true})
	manager.SetRefreshTokenCfg(&manage.RefreshingConfig{AccessTokenExp: accessLifetime, IsGenerateRefresh: true, IsRemoveRefreshing: true})
	manager.SetValidateURIHandler(func(clientID, uri string) error {
		if !contains(d.Clients[clientID].Redirects, uri) {
			return errors.ErrInvalidRedirectURI
		}
		return nil
	})
	cfg := server.NewConfig()
	cfg.AllowedGrantTypes = []oauth2.GrantType{oauth2.AuthorizationCode, oauth2.Refreshing}
	cfg.AllowedResponseTypes = []oauth2.ResponseType{oauth2.Code}
	cfg.AllowedCodeChallengeMethods = []oauth2.CodeChallengeMethod{oauth2.CodeChallengeS256}
	cfg.ForcePKCE = true
	engine := server.NewServer(cfg, manager)
	engine.SetClientInfoHandler(server.ClientFormHandler)
	engine.RefreshingValidationHandler = func(t oauth2.TokenInfo) (bool, error) {
		g, ok := d.Grants[t.GetUserID()]
		return ok && !g.Revoked && time.Now().Before(g.Expires), nil
	}
	engine.RefreshingScopeHandler = func(req *oauth2.TokenGenerateRequest, scope string) (bool, error) { return req.Scope == scope, nil }
	return manager, engine
}

type opaqueGenerate struct{}

func (opaqueGenerate) Token(_ context.Context, _ *oauth2.GenerateBasic, refresh bool) (string, string, error) {
	r := ""
	if refresh {
		r = randomID("rwrt_", 32)
	}
	return randomID("rwat_", 32), r, nil
}

// AuthorizeGenerate has a distinct method, so use a separate tiny adapter.
type codeGenerate struct{}

func (codeGenerate) Token(_ context.Context, _ *oauth2.GenerateBasic) (string, error) {
	return randomID("rwa_", 32), nil
}

type oauthAdapter struct{ d *oauthData }

func (s *oauthAdapter) GetByID(_ context.Context, id string) (oauth2.ClientInfo, error) {
	c, ok := s.d.Clients[id]
	if !ok {
		return nil, errors.ErrInvalidClient
	}
	return &models.Client{ID: c.ID, Domain: c.ID, Public: true}, nil
}
func (s *oauthAdapter) Create(_ context.Context, info oauth2.TokenInfo) error {
	t := *(info.(*models.Token))
	access, refresh, code := t.Access, t.Refresh, t.Code
	t.Access = ""
	t.Refresh = ""
	t.Code = ""
	if access != "" {
		s.d.Access[secretHash(access)] = t
	}
	if refresh != "" {
		s.d.Refresh[secretHash(refresh)] = t
	}
	if code != "" {
		s.d.Codes[secretHash(code)] = t
	}
	return nil
}
func (s *oauthAdapter) RemoveByCode(_ context.Context, code string) error {
	delete(s.d.Codes, secretHash(code))
	return nil
}
func (s *oauthAdapter) RemoveByAccess(_ context.Context, access string) error {
	delete(s.d.Access, secretHash(access))
	return nil
}
func (s *oauthAdapter) RemoveByRefresh(_ context.Context, refresh string) error {
	hash := secretHash(refresh)
	if t, ok := s.d.Refresh[hash]; ok {
		s.d.UsedRefresh[hash] = t.UserID
	}
	delete(s.d.Refresh, hash)
	return nil
}
func (s *oauthAdapter) live(t models.Token, ok bool) (oauth2.TokenInfo, error) {
	g, exists := s.d.Grants[t.UserID]
	if !ok || !exists || g.Revoked || !time.Now().Before(g.Expires) {
		return nil, nil
	}
	return &t, nil
}
func (s *oauthAdapter) GetByCode(_ context.Context, code string) (oauth2.TokenInfo, error) {
	t, ok := s.d.Codes[secretHash(code)]
	t.Code = code
	return s.live(t, ok)
}
func (s *oauthAdapter) GetByAccess(_ context.Context, access string) (oauth2.TokenInfo, error) {
	t, ok := s.d.Access[secretHash(access)]
	t.Access = access
	return s.live(t, ok)
}
func (s *oauthAdapter) GetByRefresh(_ context.Context, refresh string) (oauth2.TokenInfo, error) {
	t, ok := s.d.Refresh[secretHash(refresh)]
	t.Refresh = refresh
	return s.live(t, ok)
}
