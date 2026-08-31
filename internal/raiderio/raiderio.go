// Package raiderio is the anti-corruption layer over the Raider.IO character API.
// Raider.IO's JSON shapes never leave this package; callers only see Profile.
package raiderio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"time"
)

const defaultBaseURL = "https://raider.io"

var (
	// ErrCharacterNotFound means Raider.IO has no record of this character.
	ErrCharacterNotFound = errors.New("character not found")
	// ErrInvalidRequest means Raider.IO rejected the request itself, most likely an
	// unknown region. Distinct from ErrCharacterNotFound: the stored character may be
	// fine, so the caller must not treat its cached data as confirmed.
	ErrInvalidRequest = errors.New("invalid request")
	// ErrRateLimited means Raider.IO returned 429. The caller's tick should back off.
	ErrRateLimited = errors.New("rate limited")
	// ErrInvalidAPIKey means Raider.IO rejected the configured access key. Retrying
	// cannot fix it, so it is worth telling apart from a transient failure: every
	// request the worker makes from here on will fail the same way until the key is
	// corrected.
	ErrInvalidAPIKey = errors.New("invalid api key")
)

// Profile is our shape for a character, independent of Raider.IO's response format.
type Profile struct {
	Class           string
	Spec            string
	ItemLevel       float64
	MythicPlusScore float64
	Gear            []GearItem
	// Progression is every raid Raider.IO reported, sorted by slug. Nothing in the
	// payload says which tier is current, so picking one is the caller's decision and
	// this package refuses to make it.
	Progression []RaidProgress
}

// RaidProgress is one raid's kill counts for a character. Bosses is the raid's size,
// so a client can render "6/8" without a table of its own.
type RaidProgress struct {
	Slug         string
	Bosses       int
	NormalKilled int
	HeroicKilled int
	MythicKilled int
}

// GearItem is one equipped item slot.
type GearItem struct {
	Slot      string
	ItemID    int64
	ItemLevel int64
	EnchantID int64
	GemIDs    []int64
}

// Client fetches character profiles from Raider.IO, gated to a minimum interval
// between requests so a sync batch cannot exceed Raider.IO's rate limit.
type Client struct {
	baseURL    string
	accessKey  string
	httpClient *http.Client
	gate       <-chan time.Time
}

// NewClient builds a Client. minInterval is the minimum gap enforced between
// outgoing requests; pass 0 to disable gating (tests only).
//
// accessKey is the Raider.IO application key, empty for anonymous access. Raider.IO
// takes it as a query parameter rather than a header, which is why it never appears in
// an error here: the request URL carries the secret, and errors get logged.
func NewClient(baseURL, accessKey string, minInterval time.Duration) *Client {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}

	var gate <-chan time.Time
	if minInterval > 0 {
		gate = time.Tick(minInterval)
	}

	return &Client{
		baseURL:    baseURL,
		accessKey:  accessKey,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		gate:       gate,
	}
}

// CharacterProfile fetches gear and current-season Mythic+ score for one character.
func (c *Client) CharacterProfile(ctx context.Context, region, realm, name string) (Profile, error) {
	if c.gate != nil {
		select {
		case <-c.gate:
		case <-ctx.Done():
			return Profile{}, ctx.Err()
		}
	}

	query := url.Values{
		"region": {region},
		"realm":  {realm},
		"name":   {name},
		"fields": {"gear,mythic_plus_scores_by_season:current,raid_progression"},
	}
	if c.accessKey != "" {
		query.Set("access_key", c.accessKey)
	}
	u := c.baseURL + "/api/v1/characters/profile?" + query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Profile{}, fmt.Errorf("building request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// *url.Error prints the URL it failed on, and that URL carries the access key.
		// Unwrapping to the cause keeps the key out of the worker's logs.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return Profile{}, fmt.Errorf("calling raider.io: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Profile{}, fmt.Errorf("reading raider.io response: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return Profile{}, ErrCharacterNotFound
	case http.StatusBadRequest:
		return Profile{}, ErrInvalidRequest
	case http.StatusForbidden:
		return Profile{}, ErrInvalidAPIKey
	case http.StatusTooManyRequests:
		return Profile{}, ErrRateLimited
	default:
		return Profile{}, fmt.Errorf("raider.io returned %d", resp.StatusCode)
	}

	var raw rawProfile
	if err := json.Unmarshal(body, &raw); err != nil {
		return Profile{}, fmt.Errorf("parsing raider.io response: %w", err)
	}

	return raw.toProfile(), nil
}

// ProfileURL is the human-facing Raider.IO page for a character, for clients that
// want to link a raider's name somewhere a person will click it. Unlike the API
// endpoint above it never goes through Client.baseURL: that field exists so a test
// can point the API call at an httptest server, and a link handed to a raider has to
// resolve on the real site regardless.
func ProfileURL(region, realm, name string) string {
	return defaultBaseURL + "/characters/" +
		url.PathEscape(region) + "/" +
		url.PathEscape(realm) + "/" +
		url.PathEscape(name)
}

// rawProfile mirrors the fields we request from the Raider.IO profile endpoint.
type rawProfile struct {
	Class          string `json:"class"`
	ActiveSpecName string `json:"active_spec_name"`
	Gear           struct {
		ItemLevelEquipped float64                `json:"item_level_equipped"`
		Items             map[string]rawGearItem `json:"items"`
	} `json:"gear"`
	MythicPlusScoresBySeason []struct {
		Scores struct {
			All float64 `json:"all"`
		} `json:"scores"`
	} `json:"mythic_plus_scores_by_season"`
	// Keyed by raid slug. A character who has never set foot in a raid comes back with
	// the key absent rather than with an empty object.
	RaidProgression map[string]rawRaidProgress `json:"raid_progression"`
}

type rawRaidProgress struct {
	TotalBosses        int `json:"total_bosses"`
	NormalBossesKilled int `json:"normal_bosses_killed"`
	HeroicBossesKilled int `json:"heroic_bosses_killed"`
	MythicBossesKilled int `json:"mythic_bosses_killed"`
}

type rawGearItem struct {
	ItemID    int64   `json:"item_id"`
	ItemLevel int64   `json:"item_level"`
	Enchant   int64   `json:"enchant"`
	Gems      []int64 `json:"gems"`
}

func (r rawProfile) toProfile() Profile {
	p := Profile{
		Class:     r.Class,
		Spec:      r.ActiveSpecName,
		ItemLevel: r.Gear.ItemLevelEquipped,
	}

	if len(r.MythicPlusScoresBySeason) > 0 {
		p.MythicPlusScore = r.MythicPlusScoresBySeason[0].Scores.All
	}

	p.Gear = make([]GearItem, 0, len(r.Gear.Items))
	for slot, item := range r.Gear.Items {
		p.Gear = append(p.Gear, GearItem{
			Slot:      slot,
			ItemID:    item.ItemID,
			ItemLevel: item.ItemLevel,
			EnchantID: item.Enchant,
			GemIDs:    item.Gems,
		})
	}
	// Map iteration order is random; sort by slot so two calls with identical gear
	// produce identical marshalled bytes for the sync package's change detection.
	sort.Slice(p.Gear, func(i, j int) bool { return p.Gear[i].Slot < p.Gear[j].Slot })

	p.Progression = make([]RaidProgress, 0, len(r.RaidProgression))
	for slug, raid := range r.RaidProgression {
		p.Progression = append(p.Progression, RaidProgress{
			Slug:         slug,
			Bosses:       raid.TotalBosses,
			NormalKilled: raid.NormalBossesKilled,
			HeroicKilled: raid.HeroicBossesKilled,
			MythicKilled: raid.MythicBossesKilled,
		})
	}
	// Sorted for the same reason the gear is: the syncer compares two profiles to
	// decide whether a write is a no-op, and map order would make that a coin flip.
	sort.Slice(p.Progression, func(i, j int) bool { return p.Progression[i].Slug < p.Progression[j].Slug })

	return p
}
