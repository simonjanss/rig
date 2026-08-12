package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"golang.org/x/oauth2"

	"github.com/simonjanss/rig/runtime/rigerr"
)

// Provider is one place people can sign in from.
//
// It is a struct rather than an interface because the differences between
// providers are data — three URLs and a way to read a profile — and an
// interface would make adding one a new type instead of a literal.
type Provider struct {
	// Name must match a label of the oauth_provider enum, since it is what
	// gets stored. The built-in constructors get this right.
	Name string

	ClientID     string
	ClientSecret string
	Scopes       []string
	Endpoint     oauth2.Endpoint

	// UserInfoURL is fetched with the access token to read a profile.
	UserInfoURL string
	// Parse turns the provider's answer into a profile. Every provider spells
	// the same three facts differently, and this is where that ends.
	Parse func(body []byte) (Profile, error)

	// Extra runs after Parse when a provider needs a second call. GitHub does:
	// its user endpoint returns a null address for anybody who kept theirs
	// private.
	Extra func(ctx context.Context, client *http.Client, p *Profile) error
}

func (p Provider) config(redirectURI string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     p.ClientID,
		ClientSecret: p.ClientSecret,
		Scopes:       p.Scopes,
		Endpoint:     p.Endpoint,
		RedirectURL:  redirectURI,
	}
}

// fetch reads the profile.
func (p Provider) fetch(ctx context.Context, cfg *oauth2.Config, token *oauth2.Token) (Profile, error) {
	client := cfg.Client(ctx, token)
	client.Timeout = 10 * time.Second

	body, err := get(ctx, client, p.UserInfoURL)
	if err != nil {
		return Profile{}, rigerr.Internal(err, "%s did not return a profile", p.Name)
	}

	profile, err := p.Parse(body)
	if err != nil {
		return Profile{}, rigerr.Internal(err, "%s returned a profile rig could not read", p.Name)
	}
	if p.Extra != nil {
		if err := p.Extra(ctx, client, &profile); err != nil {
			return Profile{}, err
		}
	}
	return profile, nil
}

func get(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d from %s", res.StatusCode, url)
	}

	// Bounded, because this is a response from somewhere else.
	return io.ReadAll(io.LimitReader(res.Body, 1<<20))
}

// The provider names, which are also the oauth_provider enum's labels.
const (
	ProviderGoogle    = "Google"
	ProviderMicrosoft = "Microsoft"
	ProviderGitHub    = "GitHub"
)

// Google signs people in with a Google or Tenant account.
func Google(clientID, clientSecret string) Provider {
	return Provider{
		Name:         ProviderGoogle,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Scopes:       []string{"openid", "email", "profile"},
		Endpoint: oauth2.Endpoint{
			AuthURL:   "https://accounts.google.com/o/oauth2/v2/auth",
			TokenURL:  "https://oauth2.googleapis.com/token",
			AuthStyle: oauth2.AuthStyleInParams,
		},
		UserInfoURL: "https://openidconnect.googleapis.com/v1/userinfo",
		Parse: func(body []byte) (Profile, error) {
			var v struct {
				Sub           string `json:"sub"`
				Email         string `json:"email"`
				EmailVerified bool   `json:"email_verified"`
				Name          string `json:"name"`
			}
			if err := json.Unmarshal(body, &v); err != nil {
				return Profile{}, err
			}
			return Profile{
				Subject: v.Sub, EmailAddress: v.Email,
				EmailVerified: v.EmailVerified, DisplayName: v.Name,
			}, nil
		},
	}
}

// Microsoft signs people in with a Microsoft or Entra ID account.
//
// The tenant is Microsoft's, not rig's: "common" accepts any account,
// "organizations" excludes personal ones, and a directory identifier restricts
// sign-in to one organization. Leave it empty for "common".
func Microsoft(clientID, clientSecret, tenant string) Provider {
	if tenant == "" {
		tenant = "common"
	}
	return Provider{
		Name:         ProviderMicrosoft,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Scopes:       []string{"openid", "email", "profile"},
		Endpoint: oauth2.Endpoint{
			AuthURL:   "https://login.microsoftonline.com/" + tenant + "/oauth2/v2.0/authorize",
			TokenURL:  "https://login.microsoftonline.com/" + tenant + "/oauth2/v2.0/token",
			AuthStyle: oauth2.AuthStyleInParams,
		},
		UserInfoURL: "https://graph.microsoft.com/oidc/userinfo",
		Parse: func(body []byte) (Profile, error) {
			var v struct {
				Sub   string `json:"sub"`
				Email string `json:"email"`
				Name  string `json:"name"`
			}
			if err := json.Unmarshal(body, &v); err != nil {
				return Profile{}, err
			}
			// Microsoft does not return email_verified from this endpoint, and
			// an address in a directory it controls is one it has already
			// established. Treating it as unverified would make linking
			// impossible for every Entra customer.
			return Profile{
				Subject: v.Sub, EmailAddress: v.Email,
				EmailVerified: v.Email != "", DisplayName: v.Name,
			}, nil
		},
	}
}

// GitHub signs people in with a GitHub account.
func GitHub(clientID, clientSecret string) Provider {
	return Provider{
		Name:         ProviderGitHub,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Scopes:       []string{"read:user", "user:email"},
		Endpoint: oauth2.Endpoint{
			AuthURL:   "https://github.com/login/oauth/authorize",
			TokenURL:  "https://github.com/login/oauth/access_token",
			AuthStyle: oauth2.AuthStyleInParams,
		},
		UserInfoURL: "https://api.github.com/user",
		Parse: func(body []byte) (Profile, error) {
			var v struct {
				ID    int64  `json:"id"`
				Email string `json:"email"`
				Name  string `json:"name"`
				Login string `json:"login"`
			}
			if err := json.Unmarshal(body, &v); err != nil {
				return Profile{}, err
			}
			name := v.Name
			if name == "" {
				name = v.Login
			}
			// The numeric id, not the login: a login can be changed and reused
			// by somebody else, which is exactly the failure this package is
			// built to avoid.
			return Profile{
				Subject: fmt.Sprintf("%d", v.ID), EmailAddress: v.Email, DisplayName: name,
			}, nil
		},
		Extra: func(ctx context.Context, client *http.Client, p *Profile) error {
			// The user endpoint returns null for anybody who kept their address
			// private, and it never says whether one is verified. The addresses
			// endpoint answers both.
			body, err := get(ctx, client, "https://api.github.com/user/emails")
			if err != nil {
				return rigerr.Internal(err, "GitHub did not return an address")
			}

			var addresses []struct {
				Email    string `json:"email"`
				Primary  bool   `json:"primary"`
				Verified bool   `json:"verified"`
			}
			if err := json.Unmarshal(body, &addresses); err != nil {
				return rigerr.Internal(err, "GitHub returned addresses rig could not read")
			}

			for _, a := range addresses {
				if a.Primary && a.Verified {
					p.EmailAddress, p.EmailVerified = a.Email, true
					return nil
				}
			}
			// Fall back to any verified address before giving up: somebody with
			// no primary set still has an account worth signing in to.
			for _, a := range addresses {
				if a.Verified {
					p.EmailAddress, p.EmailVerified = a.Email, true
					return nil
				}
			}
			return nil
		},
	}
}
