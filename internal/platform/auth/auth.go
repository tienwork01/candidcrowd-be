package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/candidcrowd/candidcrowd-backend/internal/apierror"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

const IdentityKey = "authenticated_identity"

type Identity struct {
	BetterAuthUserID, Email, Name, TermsVersion, PrivacyVersion string
	EmailVerified                                               bool
}
type Verifier struct {
	keys             keyfunc.Keyfunc
	issuer, audience string
}

func New(ctx context.Context, jwks, issuer, audience string) (*Verifier, error) {
	keys, err := keyfunc.NewDefaultCtx(ctx, []string{jwks})
	if err != nil {
		return nil, err
	}
	return &Verifier{keys, issuer, audience}, nil
}

func (v *Verifier) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			apierror.Respond(c, apierror.New(http.StatusUnauthorized, "unauthenticated", "bearer token required"))
			c.Abort()
			return
		}
		claims := jwt.MapClaims{}
		opts := []jwt.ParserOption{jwt.WithValidMethods([]string{"EdDSA"})}
		if v.issuer != "" {
			opts = append(opts, jwt.WithIssuer(v.issuer))
		}
		if v.audience != "" {
			opts = append(opts, jwt.WithAudience(v.audience))
		}
		token, err := jwt.ParseWithClaims(strings.TrimPrefix(header, "Bearer "), claims, v.keys.Keyfunc, opts...)
		if err != nil || !token.Valid {
			apierror.Respond(c, apierror.New(http.StatusUnauthorized, "invalid_token", "invalid Better Auth token"))
			c.Abort()
			return
		}
		sub, _ := claims.GetSubject()
		email, _ := claims["email"].(string)
		name, _ := claims["name"].(string)
		termsVersion, _ := claims["terms_version"].(string)
		privacyVersion, _ := claims["privacy_version"].(string)
		emailVerified, _ := claims["email_verified"].(bool)
		if sub == "" {
			apierror.Respond(c, apierror.New(http.StatusUnauthorized, "invalid_token", "token has no subject"))
			c.Abort()
			return
		}
		c.Set(IdentityKey, Identity{BetterAuthUserID: sub, Email: email, Name: name, TermsVersion: termsVersion, PrivacyVersion: privacyVersion, EmailVerified: emailVerified})
		c.Next()
	}
}

func Get(c *gin.Context) (Identity, error) {
	v, ok := c.Get(IdentityKey)
	if !ok {
		return Identity{}, fmt.Errorf("authenticated identity missing")
	}
	identity, ok := v.(Identity)
	if !ok {
		return Identity{}, fmt.Errorf("invalid authenticated identity")
	}
	return identity, nil
}
