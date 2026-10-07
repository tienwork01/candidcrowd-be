package httpapi

import (
	"testing"

	"github.com/candidcrowd/candidcrowd-backend/internal/catalog"
	"github.com/candidcrowd/candidcrowd-backend/internal/livewall"
	"github.com/stretchr/testify/require"
)

func TestLiveWallFeatures(t *testing.T) {
	mode := func(m livewall.TransitionMode) *livewall.TransitionMode { return &m }
	policy := livewall.ContentPolicyFeaturedOnly
	layout := livewall.LayoutModeFeatured
	spotlight := livewall.LayoutModeSpotlight

	require.Empty(t, liveWallFeatures(liveWallUpdateRequest{TransitionMode: mode(livewall.TransitionModeClassic), LayoutMode: &spotlight}))
	require.Equal(t, []catalog.Feature{catalog.FeatureFullLiveWall}, liveWallFeatures(liveWallUpdateRequest{TransitionMode: mode(livewall.TransitionModeFloat3D)}))
	require.Equal(t, []catalog.Feature{catalog.FeatureFullLiveWall}, liveWallFeatures(liveWallUpdateRequest{TransitionMode: mode(livewall.TransitionModeCinematic)}))
	require.Equal(t, []catalog.Feature{catalog.FeatureThroughTheMoment}, liveWallFeatures(liveWallUpdateRequest{TransitionMode: mode(livewall.TransitionModeLivingMosaic)}))
	require.Equal(t, []catalog.Feature{catalog.FeatureFeatureMedia}, liveWallFeatures(liveWallUpdateRequest{ContentPolicy: &policy}))
	require.Equal(t, []catalog.Feature{catalog.FeatureFeatureMedia}, liveWallFeatures(liveWallUpdateRequest{LayoutMode: &layout}))
}
