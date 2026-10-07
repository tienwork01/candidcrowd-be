package event

import (
	"context"
	"errors"
	"testing"

	"github.com/candidcrowd/candidcrowd-backend/internal/catalog"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func TestIsBasicGuestTheme(t *testing.T) {
	cases := []struct {
		name  string
		theme string
		basic bool
	}{
		{"preset with content edits", `{"presetId":"romantic","primaryColor":"#881337","bgColor":"#FFF1F2","surfaceColor":"#ffffff","fontHeading":"serif","fontBody":"serif","heroStyle":"avatar","galleryLayout":"masonry","cameraFrame":"polaroid","welcomeMessage":"Hi!","ctaText":"Share","eventTitleOverride":"A & B","photoPrompts":["first dance"],"monogram":"AB","coverUrl":null}`, true},
		{"preset sample cover", `{"presetId":"editorial","coverUrl":"https://images.unsplash.com/photo-1519741497674-611481863552?auto=format&fit=crop&w=1200&q=80"}`, true},
		{"only content", `{"welcomeMessage":"Welcome"}`, true},
		{"custom colour", `{"presetId":"romantic","primaryColor":"#123456"}`, false},
		{"custom font", `{"presetId":"modern","fontHeading":"serif"}`, false},
		{"uploaded cover", `{"presetId":"editorial","coverUrl":"data:image/jpeg;base64,AAAA"}`, false},
		{"another preset's frame", `{"presetId":"editorial","cameraFrame":"gold"}`, false},
		{"styling without preset", `{"primaryColor":"#881337"}`, false},
		{"unknown preset", `{"presetId":"neon","primaryColor":"#ff00ff"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.basic, isBasicGuestTheme([]byte(tc.theme)))
		})
	}
}

func TestIsBasicQRConfig(t *testing.T) {
	cases := []struct {
		name  string
		qr    string
		basic bool
	}{
		{"default", `{"fgColor":"#181e17","bgColor":"#ffffff","cardBgColor":"#ffffff","dotType":"rounded","cornerSquareType":"extra-rounded","cornerDotType":"dot","logoUrl":null,"logoSize":0.25,"showFrame":true,"frameColor":"#dcded2","ctaText":"Scan me"}`, true},
		{"colour preset", `{"fgColor":"#46533a","bgColor":"#fffefa","cardBgColor":"#f8f7f2"}`, true},
		{"empty", `{}`, true},
		{"custom colours", `{"fgColor":"#000001","bgColor":"#ffffff","cardBgColor":"#ffffff"}`, false},
		{"mixed presets", `{"fgColor":"#46533a","bgColor":"#ffffff","cardBgColor":"#ffffff"}`, false},
		{"dot style", `{"dotType":"classy"}`, false},
		{"logo", `{"logoUrl":"https://cdn.example/logo.png"}`, false},
		{"frame colour", `{"frameColor":"#ff0000"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.basic, isBasicQRConfig([]byte(tc.qr)))
		})
	}
}

type denyingGate struct{ calls int }

var errNotInPlan = errors.New("not in plan")

func (g *denyingGate) Require(context.Context, uuid.UUID, catalog.Feature) error {
	g.calls++
	return errNotInPlan
}

func TestUpdateGatesOnlyFullCustomizationAndChecksOwnershipFirst(t *testing.T) {
	repo := newInMemoryEventRepo()
	gate := &denyingGate{}
	svc := NewService(repo, 1<<30).UsePlanGate(gate)
	hostID := uuid.New()
	evt, err := svc.Create(context.Background(), hostID, CreateInput{Name: "Party"})
	require.NoError(t, err)

	basic := datatypes.JSON(`{"presetId":"film","welcomeMessage":"Hello"}`)
	_, err = svc.Update(context.Background(), evt.ID, hostID, UpdateInput{GuestTheme: &basic})
	require.NoError(t, err)
	require.Zero(t, gate.calls, "basic customization never consults the plan")

	full := datatypes.JSON(`{"presetId":"film","primaryColor":"#000000"}`)
	_, err = svc.Update(context.Background(), evt.ID, hostID, UpdateInput{GuestTheme: &full})
	require.ErrorIs(t, err, errNotInPlan)

	_, err = svc.Update(context.Background(), evt.ID, uuid.New(), UpdateInput{GuestTheme: &full})
	require.ErrorIs(t, err, ErrNotFound, "another host's event is not found, not 'not in plan'")
	require.Equal(t, 1, gate.calls)
}
