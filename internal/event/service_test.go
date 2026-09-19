package event

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/platform/auth"
	"github.com/candidcrowd/candidcrowd-backend/internal/profile"
	"github.com/candidcrowd/candidcrowd-backend/internal/user"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type inMemoryEventRepo struct {
	mu     sync.RWMutex
	events map[uuid.UUID]Event
}

func newInMemoryEventRepo() *inMemoryEventRepo {
	return &inMemoryEventRepo{events: make(map[uuid.UUID]Event)}
}

func (r *inMemoryEventRepo) Create(ctx context.Context, event *Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if event.ID == uuid.Nil {
		event.ID = uuid.New()
	}
	r.events[event.ID] = *event
	return nil
}

func (r *inMemoryEventRepo) ListByHost(ctx context.Context, hostID uuid.UUID, filter ListFilter) ([]Event, int64, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var matches []Event
	q := strings.ToLower(strings.TrimSpace(filter.Query))
	for _, e := range r.events {
		if e.HostID != hostID {
			continue
		}
		if q != "" {
			nameMatch := strings.Contains(strings.ToLower(e.Name), q)
			typeMatch := strings.Contains(strings.ToLower(e.EventType), q)
			if !nameMatch && !typeMatch {
				continue
			}
		}
		if filter.EventType != "" && !strings.EqualFold(e.EventType, filter.EventType) {
			continue
		}
		matches = append(matches, e)
	}

	total := int64(len(matches))

	direction := strings.ToLower(strings.TrimSpace(filter.Direction))
	sortField := strings.ToLower(strings.TrimSpace(filter.Sort))

	switch sortField {
	case "name":
		sort.Slice(matches, func(i, j int) bool {
			if direction == "desc" {
				return matches[i].Name > matches[j].Name
			}
			return matches[i].Name < matches[j].Name
		})
	case "upcoming", "event_date":
		now := time.Now()
		sort.Slice(matches, func(i, j int) bool {
			iDate := matches[i].EventDate
			jDate := matches[j].EventDate
			if direction == "desc" {
				if iDate == nil {
					return false
				}
				if jDate == nil {
					return true
				}
				return iDate.After(*jDate)
			}
			iUpcoming := iDate != nil && !iDate.Before(now)
			jUpcoming := jDate != nil && !jDate.Before(now)
			if iUpcoming && jUpcoming {
				return iDate.Before(*jDate)
			}
			if iUpcoming {
				return true
			}
			if jUpcoming {
				return false
			}
			return matches[i].CreatedAt.After(matches[j].CreatedAt)
		})
	case "oldest":
		sort.Slice(matches, func(i, j int) bool {
			return matches[i].CreatedAt.Before(matches[j].CreatedAt)
		})
	case "newest":
		sort.Slice(matches, func(i, j int) bool {
			return matches[i].CreatedAt.After(matches[j].CreatedAt)
		})
	default: // "created_at" or unspecified
		sort.Slice(matches, func(i, j int) bool {
			if direction == "asc" {
				return matches[i].CreatedAt.Before(matches[j].CreatedAt)
			}
			return matches[i].CreatedAt.After(matches[j].CreatedAt)
		})
	}

	page := filter.Page
	if page < 1 {
		page = 1
	}
	perPage := filter.PerPage
	if perPage < 1 {
		perPage = 12
	}

	start := (page - 1) * perPage
	if start >= len(matches) {
		return []Event{}, total, nil
	}
	end := start + perPage
	if end > len(matches) {
		end = len(matches)
	}

	res := make([]Event, end-start)
	copy(res, matches[start:end])
	return res, total, nil
}

func (r *inMemoryEventRepo) GetOwned(ctx context.Context, id, hostID uuid.UUID) (Event, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.events[id]
	if !ok || e.HostID != hostID {
		return Event{}, gorm.ErrRecordNotFound
	}
	return e, nil
}

func (r *inMemoryEventRepo) GetPublic(ctx context.Context, slug string) (Event, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, e := range r.events {
		if e.Slug == slug && e.Status == StatusActive {
			return e, nil
		}
	}
	return Event{}, gorm.ErrRecordNotFound
}

func TestEventCreateAndIsolation(t *testing.T) {
	repo := newInMemoryEventRepo()
	svc := NewService(repo, 100*1024*1024)
	ctx := context.Background()

	host1ID := uuid.New()
	host2ID := uuid.New()

	// Host 1 creates an event
	e1, err := svc.Create(ctx, host1ID, CreateInput{
		Name:               "Host 1 Wedding",
		EventType:          "Wedding",
		ExpectedGuestCount: 50,
	})
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, e1.ID)
	require.Equal(t, "Host 1 Wedding", e1.Name)
	require.Equal(t, StatusActive, e1.Status)
	require.NotEmpty(t, e1.Slug)

	// Host 2 creates an event
	e2, err := svc.Create(ctx, host2ID, CreateInput{
		Name:               "Host 2 Birthday",
		EventType:          "Birthday",
		ExpectedGuestCount: 20,
	})
	require.NoError(t, err)

	// Host 1 lists events: only sees Host 1 event
	out1, err := svc.List(ctx, host1ID, ListInput{})
	require.NoError(t, err)
	require.NotEmpty(t, out1.Events)
	require.Equal(t, int64(1), out1.Pagination.Total)
	for _, item := range out1.Events {
		require.Equal(t, e1.HostID, item.HostID)
		require.NotEqual(t, e2.ID, item.ID)
	}

	// Host 1 can get Host 1 event
	owned, err := svc.GetOwned(ctx, e1.ID, host1ID)
	require.NoError(t, err)
	require.Equal(t, e1.ID, owned.ID)

	// Host 2 CANNOT get Host 1 event (returns record not found)
	_, err = svc.GetOwned(ctx, e1.ID, host2ID)
	require.Error(t, err)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

func TestEventCreateValidation(t *testing.T) {
	repo := newInMemoryEventRepo()
	svc := NewService(repo, 100*1024*1024)
	ctx := context.Background()

	hostID := uuid.New()

	// Empty name fails
	_, err := svc.Create(ctx, hostID, CreateInput{
		Name:               "   ",
		ExpectedGuestCount: 10,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "event name is required")

	// Negative guest count fails
	_, err = svc.Create(ctx, hostID, CreateInput{
		Name:               "Valid Name",
		ExpectedGuestCount: -5,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "expected guest count cannot be negative")
}

func TestEventHandlerCreateGate(t *testing.T) {
	eventRepo := newInMemoryEventRepo()
	eventSvc := NewService(eventRepo, 100*1024*1024)

	// In-memory profile service for the gate test
	profileSvc := profile.NewService(newTestProfileRepo(), "2026-01", "2026-01")
	handler := NewHandler(eventSvc, profileSvc)

	gin.SetMode(gin.TestMode)

	makeRouter := func(id auth.Identity) *gin.Engine {
		r := gin.New()
		r.Use(func(c *gin.Context) {
			c.Set(auth.IdentityKey, id)
			c.Next()
		})
		r.POST("/events", handler.Create)
		r.GET("/events/:id", handler.Get)
		return r
	}

	hostID := "gate_host_" + uuid.NewString()

	// 1. Unverified email returns 403 email_unverified
	unverifiedIdentity := auth.Identity{
		BetterAuthUserID: hostID,
		Email:            hostID + "@example.com",
		Name:             "Unverified Host",
		EmailVerified:    false,
	}
	r1 := makeRouter(unverifiedIdentity)

	body, _ := json.Marshal(map[string]any{
		"name":                 "Celebration",
		"expected_guest_count": 25,
	})
	req1 := httptest.NewRequest(http.MethodPost, "/events", bytes.NewReader(body))
	req1.Header.Set("Content-Type", "application/json")
	res1 := httptest.NewRecorder()
	r1.ServeHTTP(res1, req1)
	require.Equal(t, http.StatusForbidden, res1.Code)
	require.Contains(t, res1.Body.String(), "email_unverified")

	// 2. Verified email without consent returns 403 consent_required
	verifiedNoConsentIdentity := auth.Identity{
		BetterAuthUserID: hostID,
		Email:            hostID + "@example.com",
		Name:             "Verified Host",
		EmailVerified:    true,
	}
	r2 := makeRouter(verifiedNoConsentIdentity)

	req2 := httptest.NewRequest(http.MethodPost, "/events", bytes.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	res2 := httptest.NewRecorder()
	r2.ServeHTTP(res2, req2)
	require.Equal(t, http.StatusForbidden, res2.Code)
	require.Contains(t, res2.Body.String(), "consent_required")

	// 3. Verified email + consent succeeds with 201 Created
	ctx := context.Background()
	_, err := profileSvc.Accept(ctx, verifiedNoConsentIdentity, "2026-01", "2026-01")
	require.NoError(t, err)

	req3 := httptest.NewRequest(http.MethodPost, "/events", bytes.NewReader(body))
	req3.Header.Set("Content-Type", "application/json")
	res3 := httptest.NewRecorder()
	r2.ServeHTTP(res3, req3)
	require.Equal(t, http.StatusCreated, res3.Code)

	var created Event
	require.NoError(t, json.Unmarshal(res3.Body.Bytes(), &created))
	require.Equal(t, "Celebration", created.Name)

	// 4. Host A requests Host B's event via Handler -> 404
	attackerIdentity := auth.Identity{
		BetterAuthUserID: "attacker_" + uuid.NewString(),
		Email:            "attacker@example.com",
	}
	rAttacker := makeRouter(attackerIdentity)
	reqGet := httptest.NewRequest(http.MethodGet, "/events/"+created.ID.String(), nil)
	resGet := httptest.NewRecorder()
	rAttacker.ServeHTTP(resGet, reqGet)
	require.Equal(t, http.StatusNotFound, resGet.Code)
}

func TestEventListPagination(t *testing.T) {
	repo := newInMemoryEventRepo()
	svc := NewService(repo, 100*1024*1024)
	ctx := context.Background()
	hostID := uuid.New()

	for i := 1; i <= 15; i++ {
		_, err := svc.Create(ctx, hostID, CreateInput{
			Name:               fmt.Sprintf("Event %02d", i),
			EventType:          "Wedding",
			ExpectedGuestCount: 10,
		})
		require.NoError(t, err)
	}

	// Page 1 with per_page 10
	p1, err := svc.List(ctx, hostID, ListInput{Page: 1, PerPage: 10})
	require.NoError(t, err)
	require.Len(t, p1.Events, 10)
	require.Equal(t, int64(15), p1.Pagination.Total)
	require.Equal(t, 2, p1.Pagination.TotalPages)
	require.Equal(t, 1, p1.Pagination.Page)
	require.Equal(t, 10, p1.Pagination.PerPage)
	require.True(t, p1.Pagination.HasNext)
	require.False(t, p1.Pagination.HasPrev)

	// Page 2 with per_page 10
	p2, err := svc.List(ctx, hostID, ListInput{Page: 2, PerPage: 10})
	require.NoError(t, err)
	require.Len(t, p2.Events, 5)
	require.Equal(t, int64(15), p2.Pagination.Total)
	require.Equal(t, 2, p2.Pagination.TotalPages)
	require.Equal(t, 2, p2.Pagination.Page)
	require.False(t, p2.Pagination.HasNext)
	require.True(t, p2.Pagination.HasPrev)
}

func TestEventListSearchAndFilter(t *testing.T) {
	repo := newInMemoryEventRepo()
	svc := NewService(repo, 100*1024*1024)
	ctx := context.Background()
	hostID := uuid.New()

	_, err := svc.Create(ctx, hostID, CreateInput{Name: "Grand Wedding Gala", EventType: "Wedding"})
	require.NoError(t, err)
	_, err = svc.Create(ctx, hostID, CreateInput{Name: "Summer Beach Party", EventType: "Birthday"})
	require.NoError(t, err)
	_, err = svc.Create(ctx, hostID, CreateInput{Name: "Company Annual Summit", EventType: "Conference"})
	require.NoError(t, err)

	// Search by name
	res, err := svc.List(ctx, hostID, ListInput{Query: "beach"})
	require.NoError(t, err)
	require.Len(t, res.Events, 1)
	require.Equal(t, "Summer Beach Party", res.Events[0].Name)

	// Search by event_type keyword
	res, err = svc.List(ctx, hostID, ListInput{Query: "conference"})
	require.NoError(t, err)
	require.Len(t, res.Events, 1)
	require.Equal(t, "Company Annual Summit", res.Events[0].Name)

	// Filter by exact type
	res, err = svc.List(ctx, hostID, ListInput{EventType: "Wedding"})
	require.NoError(t, err)
	require.Len(t, res.Events, 1)
	require.Equal(t, "Grand Wedding Gala", res.Events[0].Name)

	// Non-matching search
	res, err = svc.List(ctx, hostID, ListInput{Query: "nonexistent"})
	require.NoError(t, err)
	require.Empty(t, res.Events)
	require.Equal(t, int64(0), res.Pagination.Total)
}

func TestEventListSort(t *testing.T) {
	repo := newInMemoryEventRepo()
	svc := NewService(repo, 100*1024*1024)
	ctx := context.Background()
	hostID := uuid.New()

	_, err := svc.Create(ctx, hostID, CreateInput{Name: "Bravo Event"})
	require.NoError(t, err)
	_, err = svc.Create(ctx, hostID, CreateInput{Name: "Alpha Event"})
	require.NoError(t, err)
	_, err = svc.Create(ctx, hostID, CreateInput{Name: "Charlie Event"})
	require.NoError(t, err)

	// Sort by name asc (default for name)
	res, err := svc.List(ctx, hostID, ListInput{Sort: "name", Direction: "asc"})
	require.NoError(t, err)
	require.Len(t, res.Events, 3)
	require.Equal(t, "Alpha Event", res.Events[0].Name)
	require.Equal(t, "Bravo Event", res.Events[1].Name)
	require.Equal(t, "Charlie Event", res.Events[2].Name)

	// Sort by name desc
	resDesc, err := svc.List(ctx, hostID, ListInput{Sort: "name", Direction: "desc"})
	require.NoError(t, err)
	require.Len(t, resDesc.Events, 3)
	require.Equal(t, "Charlie Event", resDesc.Events[0].Name)
	require.Equal(t, "Bravo Event", resDesc.Events[1].Name)
	require.Equal(t, "Alpha Event", resDesc.Events[2].Name)
}

func TestEventHandlerListWithQuery(t *testing.T) {
	eventRepo := newInMemoryEventRepo()
	eventSvc := NewService(eventRepo, 100*1024*1024)
	profileSvc := profile.NewService(newTestProfileRepo(), "2026-01", "2026-01")
	handler := NewHandler(eventSvc, profileSvc)

	gin.SetMode(gin.TestMode)
	hostID := "host_query_" + uuid.NewString()
	identity := auth.Identity{
		BetterAuthUserID: hostID,
		Email:            hostID + "@example.com",
		Name:             "Query Host",
		EmailVerified:    true,
	}

	ctx := context.Background()
	_, err := profileSvc.Accept(ctx, identity, "2026-01", "2026-01")
	require.NoError(t, err)

	userView, err := profileSvc.Me(ctx, identity)
	require.NoError(t, err)

	// Create events for this user
	_, err = eventSvc.Create(ctx, userView.ID, CreateInput{Name: "Alpha Wedding", EventType: "Wedding"})
	require.NoError(t, err)
	_, err = eventSvc.Create(ctx, userView.ID, CreateInput{Name: "Beta Birthday", EventType: "Birthday"})
	require.NoError(t, err)

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(auth.IdentityKey, identity)
		c.Next()
	})
	r.GET("/events", handler.List)

	// Request with query params including direction=asc
	req := httptest.NewRequest(http.MethodGet, "/events?page=1&per_page=10&q=wedding&type=Wedding&sort=name&direction=asc", nil)
	res := httptest.NewRecorder()
	r.ServeHTTP(res, req)

	require.Equal(t, http.StatusOK, res.Code)

	var resp struct {
		Data       []Event    `json:"data"`
		Pagination Pagination `json:"pagination"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &resp))
	require.Len(t, resp.Data, 1)
	require.Equal(t, "Alpha Wedding", resp.Data[0].Name)
	require.Equal(t, int64(1), resp.Pagination.Total)
	require.Equal(t, 1, resp.Pagination.Page)
	require.Equal(t, 10, resp.Pagination.PerPage)
}

type testProfileRepo struct {
	mu       sync.RWMutex
	users    map[string]uuid.UUID
	consents map[uuid.UUID]bool
}

func newTestProfileRepo() profile.Repository {
	return &testProfileRepo{
		users:    make(map[string]uuid.UUID),
		consents: make(map[uuid.UUID]bool),
	}
}

func (r *testProfileRepo) EnsureUser(ctx context.Context, identity auth.Identity) (user.User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.users[identity.BetterAuthUserID]
	if !ok {
		id = uuid.New()
		r.users[identity.BetterAuthUserID] = id
	}
	var verifiedAt *time.Time
	if identity.EmailVerified {
		now := time.Now().UTC()
		verifiedAt = &now
	}
	return user.User{
		ID:               id,
		BetterAuthUserID: identity.BetterAuthUserID,
		Email:            identity.Email,
		DisplayName:      identity.Name,
		EmailVerifiedAt:  verifiedAt,
	}, nil
}

func (r *testProfileRepo) SaveConsents(ctx context.Context, consents []profile.Consent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range consents {
		r.consents[c.UserID] = true
	}
	return nil
}

func (r *testProfileRepo) HasRequiredConsents(ctx context.Context, userID uuid.UUID, termsVersion, privacyVersion string) (bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.consents[userID], nil
}
