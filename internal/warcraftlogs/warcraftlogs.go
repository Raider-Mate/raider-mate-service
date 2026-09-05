// Package warcraftlogs is the anti-corruption layer over the WarcraftLogs v2 API.
// WarcraftLogs' JSON shapes never leave this package; callers only see Report.
package warcraftlogs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	// ErrReportNotFound means WarcraftLogs has no report with this code. A deleted
	// report and a mistyped code are the same answer, and the raid lead has to fix the
	// link either way.
	ErrReportNotFound = errors.New("report not found")
	// ErrReportPrivate means the report exists but client credentials cannot read it.
	// Reaching a private report needs the report owner's own OAuth consent, which this
	// service never asks for, so the recovery is the raid lead setting the report to
	// unlisted on the WarcraftLogs site.
	ErrReportPrivate = errors.New("report is private")
	// ErrReportArchived means WarcraftLogs has archived the report's detail data.
	// Fights still read; the tables do not, without a WarcraftLogs subscription on the
	// reading account.
	ErrReportArchived = errors.New("report is archived")
	// ErrRateLimited means the hourly point budget is spent. The caller's tick should
	// back off until the budget resets.
	ErrRateLimited = errors.New("rate limited")
	// ErrInvalidCredentials means WarcraftLogs rejected the configured client id and
	// key. Retrying cannot fix it, so it is worth telling apart from WarcraftLogs being
	// down: every request from here on fails the same way until they are corrected.
	ErrInvalidCredentials = errors.New("invalid credentials")
)

// Report is one WarcraftLogs report as this service cares about it.
type Report struct {
	Code     string
	Title    string
	StartsAt time.Time
	EndsAt   time.Time
	// Revision goes up every time WarcraftLogs reprocesses the report. Two fetches that
	// return the same revision returned the same numbers, which is how the ingestor
	// knows a raid night has stopped moving and it can stop asking.
	Revision int32
	// Visibility is WarcraftLogs' own word: public, unlisted or private.
	Visibility string
	ZoneName   *string
	Region     *string
	// Live is true while any pull is still in progress. The numbers are real but not
	// final, and a client that does not say so will be asked why they changed.
	Live    bool
	Fights  []Fight
	Raiders []Actor
	// PerFight is the same numbers cut by pull, keyed by fight id. Empty when the report
	// had no boss pulls, or when reading them was skipped.
	PerFight map[int32][]Actor
}

// Fight is one boss pull. Trash is not a fight here: a raid night is measured in pulls,
// and the run to the instance is not one.
type Fight struct {
	ID          int32
	EncounterID int32
	Name        string
	// Difficulty is WarcraftLogs' own integer. Left raw rather than mapped: this package
	// translates shapes, not game vocabulary.
	Difficulty *int32
	Size       *int32
	Kill       bool
	// BossPercentage is how much health the boss had left, on a 0 to 100 scale, and
	// about zero on a kill. Absent on a fight WarcraftLogs could not measure.
	BossPercentage  *float64
	FightPercentage *float64
	StartsAt        time.Time
	EndsAt          time.Time
	InProgress      bool
}

// Actor is one player in the log, with their totals over the night. Name and Server are
// as WarcraftLogs recorded them; matching them to a character is not this package's job.
type Actor struct {
	ID      int32
	Name    string
	Server  string
	Class   string
	Damage  int64
	Healing int64
	Deaths  int32
}

// Budget is what is left of the hourly point allowance, read back on every fetch so the
// caller can stop before it earns a 429. WarcraftLogs does not publish the cost of a
// query, so this is measured rather than assumed.
type Budget struct {
	LimitPerHour  int32
	SpentThisHour float64
	ResetIn       time.Duration
}

// Spent reports whether the budget is used up past the given share, 0 to 1.
func (b Budget) Spent(share float64) bool {
	if b.LimitPerHour <= 0 {
		return false
	}
	return b.SpentThisHour >= float64(b.LimitPerHour)*share
}

// Client fetches reports from WarcraftLogs, gated to a minimum interval between
// requests.
//
// Unlike the Raider.IO client this one holds no single base URL: the host comes from
// the report being fetched, because classic and fresh are separate sites. Tokens are
// cached per host for the same reason.
type Client struct {
	clientID   string
	clientKey  string
	baseOverr  string
	httpClient *http.Client
	gate       <-chan time.Time

	mu     sync.Mutex
	tokens map[string]token
}

type token struct {
	value     string
	expiresAt time.Time
}

// NewClient builds a Client. minInterval is the minimum gap enforced between outgoing
// requests; pass 0 to disable gating (tests only).
//
// baseOverride replaces the report's own host when set, which is how a test points this
// at an httptest server. Empty in production, where the host is the report's.
func NewClient(clientID, clientKey, baseOverride string, minInterval time.Duration) *Client {
	var gate <-chan time.Time
	if minInterval > 0 {
		gate = time.Tick(minInterval)
	}

	return &Client{
		clientID:   clientID,
		clientKey:  clientKey,
		baseOverr:  strings.TrimSuffix(baseOverride, "/"),
		httpClient: &http.Client{Timeout: 30 * time.Second},
		gate:       gate,
		tokens:     map[string]token{},
	}
}

// reportQuery is sent as-is. Every field in it is read; nothing is requested for later.
//
// killType: Encounters everywhere, because counting the run to the instance as damage
// done would put whoever pulled the trash pack at the top of the board.
//
// The tables need a window as well as a kill type: WarcraftLogs rejects a table query
// carrying neither fightIDs nor startTime and endTime. Zero to the report's own end
// covers the night, and killType still narrows it to boss pulls.
//
// allowUnlisted: true, because the code was pasted by that guild's own raid lead and is
// read back only inside that guild. The advice to set it false is for applications that
// cache codes and hand them to strangers.
//
// rateLimitData rides along in the same document: one round trip instead of two, and the
// caller learns what the query cost at the moment it paid it.
const reportQuery = `query RaidNight($code: String!, $end: Float!) {
  rateLimitData { limitPerHour pointsSpentThisHour pointsResetIn }
  reportData {
    report(code: $code, allowUnlisted: true) {
      code title startTime endTime revision visibility
      zone { name }
      region { compactName }
      archiveStatus { isArchived isAccessible }
      masterData(translate: true) {
        actors(type: "Player") { id name server subType }
      }
      fights(killType: Encounters, translate: true) {
        id encounterID name difficulty size kill
        bossPercentage fightPercentage startTime endTime inProgress
      }
      damage:  table(dataType: DamageDone, killType: Encounters, hostilityType: Friendlies, startTime: 0, endTime: $end)
      healing: table(dataType: Healing,    killType: Encounters, hostilityType: Friendlies, startTime: 0, endTime: $end)
      deaths:  table(dataType: Deaths,     killType: Encounters, hostilityType: Friendlies, startTime: 0, endTime: $end)
    }
  }
}`

// windowEnd is the upper bound handed to the table queries, in milliseconds from the
// report's start. WarcraftLogs clamps it to the report, so anything past the longest
// imaginable raid night reads as "all of it". Twenty days.
const windowEnd = 20 * 24 * 60 * 60 * 1000

// Fetch reads one report and everything this service shows from it, in a single
// request. The Budget comes back even on success, so a caller can stop before the next.
func (c *Client) Fetch(ctx context.Context, ref ReportRef) (Report, Budget, error) {
	if c.gate != nil {
		select {
		case <-c.gate:
		case <-ctx.Done():
			return Report{}, Budget{}, ctx.Err()
		}
	}

	body, budget, err := c.graphql(ctx, ref.Host, reportQuery, map[string]any{
		"code": ref.Code,
		"end":  windowEnd,
	})
	if err != nil {
		return Report{}, budget, err
	}

	if body.Data.ReportData.Report == nil {
		// WarcraftLogs answers 200 with a null report for both "no such code" and a
		// report client credentials may not read. The errors array is what tells them
		// apart, and neither is a transport failure.
		if body.mentions("private") {
			return Report{}, budget, ErrReportPrivate
		}
		return Report{}, budget, ErrReportNotFound
	}

	raw := body.Data.ReportData.Report
	if strings.EqualFold(raw.Visibility, "private") {
		return Report{}, budget, ErrReportPrivate
	}
	if raw.ArchiveStatus != nil && raw.ArchiveStatus.IsArchived && !raw.ArchiveStatus.IsAccessible {
		return Report{}, budget, ErrReportArchived
	}

	report := raw.toReport()

	// A second document for the per-pull cut. Separate because the fight ids it aliases
	// on are only known once the first response is parsed, and one request per pull would
	// be thirty round trips for a raid night.
	perFight, perFightBudget, err := c.fetchPerFight(ctx, ref.Host, ref.Code, report.Fights)
	if err != nil {
		// The night total is the answer this feature was built for and it is already in
		// hand. Losing the per-pull cut to a rate limit is worth far less than losing the
		// report, so this degrades rather than fails.
		return report, budget, nil //nolint:nilerr
	}
	report.PerFight = perFight
	if perFightBudget.LimitPerHour > 0 {
		budget = perFightBudget
	}

	return report, budget, nil
}

// maxFightsRead caps how many pulls are read individually in one document.
//
// A thirty-pull night is three aliased tables per pull, and a GraphQL document with ninety
// table fields in it is both slow and expensive against an hourly point budget. Past this
// the night total is still there and still complete; what is lost is the per-pull cut of
// the tail of a very long night.
const maxFightsRead = 24

// fetchPerFight reads damage, healing and deaths for each pull, in one document.
//
// Aliased per fight rather than one request per pull: thirty round trips to WarcraftLogs
// for one raid night would be slower than the night was.
func (c *Client) fetchPerFight(ctx context.Context, host, code string, fights []Fight) (map[int32][]Actor, Budget, error) {
	if len(fights) == 0 {
		return nil, Budget{}, nil
	}
	if len(fights) > maxFightsRead {
		fights = fights[:maxFightsRead]
	}

	var query strings.Builder
	query.WriteString("query PerFight($code: String!) {\n")
	query.WriteString("  rateLimitData { limitPerHour pointsSpentThisHour pointsResetIn }\n")
	query.WriteString("  reportData {\n    report(code: $code, allowUnlisted: true) {\n")
	for _, fight := range fights {
		// The alias carries the fight id, which is how the response is put back together
		// without a second lookup. Fight ids are integers from WarcraftLogs, so they are
		// safe inside an alias without escaping.
		fmt.Fprintf(&query,
			"      d%d: table(dataType: DamageDone, fightIDs: [%d], hostilityType: Friendlies)\n"+
				"      h%d: table(dataType: Healing, fightIDs: [%d], hostilityType: Friendlies)\n"+
				"      x%d: table(dataType: Deaths, fightIDs: [%d], hostilityType: Friendlies)\n",
			fight.ID, fight.ID, fight.ID, fight.ID, fight.ID, fight.ID)
	}
	query.WriteString("    }\n  }\n}")

	raw, budget, err := c.graphqlRaw(ctx, host, query.String(), map[string]any{"code": code})
	if err != nil {
		return nil, budget, err
	}

	var envelope struct {
		Data struct {
			ReportData struct {
				Report map[string]json.RawMessage `json:"report"`
			} `json:"reportData"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, budget, fmt.Errorf("parsing per-fight response: %w", err)
	}

	perFight := map[int32][]Actor{}
	for _, fight := range fights {
		byID := map[int32]*Actor{}
		read := func(prefix string, apply func(actor *Actor, entry rawEntry)) {
			field, ok := envelope.Data.ReportData.Report[fmt.Sprintf("%s%d", prefix, fight.ID)]
			if !ok {
				return
			}
			var table rawTable
			if err := json.Unmarshal(field, &table); err != nil {
				return
			}
			for _, entry := range table.Data.Entries {
				actor, seen := byID[entry.ID]
				if !seen {
					actor = &Actor{ID: entry.ID, Name: entry.Name, Class: entry.Type}
					byID[entry.ID] = actor
				}
				apply(actor, entry)
			}
		}

		read("d", func(actor *Actor, entry rawEntry) { actor.Damage = entry.Total })
		read("h", func(actor *Actor, entry rawEntry) { actor.Healing = entry.Total })
		// One row per death, so this counts rows rather than reading a total.
		read("x", func(actor *Actor, _ rawEntry) { actor.Deaths++ })

		actors := make([]Actor, 0, len(byID))
		for _, actor := range byID {
			actors = append(actors, *actor)
		}
		sort.Slice(actors, func(i, j int) bool { return actors[i].ID < actors[j].ID })
		if len(actors) > 0 {
			perFight[fight.ID] = actors
		}
	}

	return perFight, budget, nil
}

// graphqlRaw posts one document and returns the undecoded body, for callers whose shape
// is built at runtime and cannot be a struct.
func (c *Client) graphqlRaw(ctx context.Context, host, query string, vars map[string]any) ([]byte, Budget, error) {
	payload, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return nil, Budget{}, fmt.Errorf("building request: %w", err)
	}

	accessToken, err := c.token(ctx, host)
	if err != nil {
		return nil, Budget{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base(host)+"/api/v2/client", strings.NewReader(string(payload)))
	if err != nil {
		return nil, Budget{}, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, Budget{}, fmt.Errorf("calling warcraftlogs: %w", unwrapURLError(err))
	}
	defer resp.Body.Close() //nolint:errcheck

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, Budget{}, fmt.Errorf("reading warcraftlogs response: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		c.forget(host)
		return nil, Budget{}, ErrInvalidCredentials
	case http.StatusTooManyRequests:
		return nil, Budget{}, ErrRateLimited
	default:
		return nil, Budget{}, fmt.Errorf("warcraftlogs returned %d", resp.StatusCode)
	}

	var limits struct {
		Data struct {
			RateLimitData rawRateLimit `json:"rateLimitData"`
		} `json:"data"`
	}
	_ = json.Unmarshal(body, &limits)

	return body, limits.Data.RateLimitData.toBudget(), nil
}

// graphql posts one document and returns the decoded envelope.
func (c *Client) graphql(ctx context.Context, host, query string, vars map[string]any) (rawEnvelope, Budget, error) {
	var envelope rawEnvelope

	accessToken, err := c.token(ctx, host)
	if err != nil {
		return envelope, Budget{}, err
	}

	payload, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return envelope, Budget{}, fmt.Errorf("building request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base(host)+"/api/v2/client", strings.NewReader(string(payload)))
	if err != nil {
		return envelope, Budget{}, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return envelope, Budget{}, fmt.Errorf("calling warcraftlogs: %w", unwrapURLError(err))
	}
	defer resp.Body.Close() //nolint:errcheck

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return envelope, Budget{}, fmt.Errorf("reading warcraftlogs response: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		// The cached token is the likeliest cause, and it is cheap to drop. The caller
		// retrying gets a fresh one; a genuinely bad credential fails at the token
		// exchange instead, where it is reported as such.
		c.forget(host)
		return envelope, Budget{}, ErrInvalidCredentials
	case http.StatusTooManyRequests:
		return envelope, Budget{}, ErrRateLimited
	default:
		return envelope, Budget{}, fmt.Errorf("warcraftlogs returned %d", resp.StatusCode)
	}

	if err := json.Unmarshal(raw, &envelope); err != nil {
		return envelope, Budget{}, fmt.Errorf("parsing warcraftlogs response: %w", err)
	}

	budget := envelope.Data.RateLimitData.toBudget()

	// A GraphQL error arrives inside a 200. Only the ones that took the whole document
	// down matter here: a null report is a real answer and is read by the caller.
	if len(envelope.Errors) > 0 && envelope.Data.ReportData.Report == nil {
		if envelope.mentions("rate limit") {
			return envelope, budget, ErrRateLimited
		}
	}

	return envelope, budget, nil
}

// token returns a cached access token for a host, exchanging credentials when the
// cached one is missing or close enough to expiry to be worth replacing.
func (c *Client) token(ctx context.Context, host string) (string, error) {
	c.mu.Lock()
	cached, ok := c.tokens[host]
	c.mu.Unlock()
	if ok && time.Now().Before(cached.expiresAt) {
		return cached.value, nil
	}

	form := url.Values{"grant_type": {"client_credentials"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base(host)+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("building token request: %w", err)
	}
	req.SetBasicAuth(c.clientID, c.clientKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("calling warcraftlogs: %w", unwrapURLError(err))
	}
	defer resp.Body.Close() //nolint:errcheck

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading token response: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusBadRequest:
		return "", ErrInvalidCredentials
	case http.StatusTooManyRequests:
		return "", ErrRateLimited
	default:
		return "", fmt.Errorf("warcraftlogs token endpoint returned %d", resp.StatusCode)
	}

	var granted struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &granted); err != nil {
		return "", fmt.Errorf("parsing token response: %w", err)
	}
	if granted.AccessToken == "" {
		return "", ErrInvalidCredentials
	}

	lifetime := time.Duration(granted.ExpiresIn) * time.Second
	// A minute of margin, so a token that expires mid-flight is replaced before the
	// request rather than after it fails.
	if lifetime > time.Minute {
		lifetime -= time.Minute
	}

	c.mu.Lock()
	c.tokens[host] = token{value: granted.AccessToken, expiresAt: time.Now().Add(lifetime)}
	c.mu.Unlock()

	return granted.AccessToken, nil
}

func (c *Client) forget(host string) {
	c.mu.Lock()
	delete(c.tokens, host)
	c.mu.Unlock()
}

func (c *Client) base(host string) string {
	if c.baseOverr != "" {
		return c.baseOverr
	}
	return "https://" + host
}

// unwrapURLError strips the *url.Error wrapper, which prints the URL it failed on.
// Nothing in a WarcraftLogs URL is secret today, but the credentials ride in a header
// on the same client and an unwrapped error is one refactor away from carrying them.
func unwrapURLError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}

// rawEnvelope mirrors the GraphQL response. Nothing below this line leaves the package.
type rawEnvelope struct {
	Data struct {
		RateLimitData rawRateLimit `json:"rateLimitData"`
		ReportData    struct {
			Report *rawReport `json:"report"`
		} `json:"reportData"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// mentions reports whether any GraphQL error carries the given word. WarcraftLogs has
// no error codes, so the message is the only signal there is.
func (e rawEnvelope) mentions(word string) bool {
	for _, err := range e.Errors {
		if strings.Contains(strings.ToLower(err.Message), word) {
			return true
		}
	}
	return false
}

type rawRateLimit struct {
	LimitPerHour        int32   `json:"limitPerHour"`
	PointsSpentThisHour float64 `json:"pointsSpentThisHour"`
	PointsResetIn       int64   `json:"pointsResetIn"`
}

func (r rawRateLimit) toBudget() Budget {
	return Budget{
		LimitPerHour:  r.LimitPerHour,
		SpentThisHour: r.PointsSpentThisHour,
		ResetIn:       time.Duration(r.PointsResetIn) * time.Second,
	}
}

type rawReport struct {
	Code string `json:"code"`
	// Title is empty on plenty of real reports, so it is never assumed to be a name.
	Title string `json:"title"`
	// StartTime and EndTime are Unix milliseconds. Fight times are milliseconds
	// relative to StartTime, which is the trap in this whole package.
	StartTime  float64 `json:"startTime"`
	EndTime    float64 `json:"endTime"`
	Revision   int32   `json:"revision"`
	Visibility string  `json:"visibility"`
	Zone       *struct {
		Name string `json:"name"`
	} `json:"zone"`
	Region *struct {
		CompactName string `json:"compactName"`
	} `json:"region"`
	ArchiveStatus *struct {
		IsArchived   bool `json:"isArchived"`
		IsAccessible bool `json:"isAccessible"`
	} `json:"archiveStatus"`
	MasterData struct {
		Actors []rawActor `json:"actors"`
	} `json:"masterData"`
	Fights  []rawFight `json:"fights"`
	Damage  rawTable   `json:"damage"`
	Healing rawTable   `json:"healing"`
	Deaths  rawTable   `json:"deaths"`
}

type rawActor struct {
	ID     int32  `json:"id"`
	Name   string `json:"name"`
	Server string `json:"server"`
	// SubType is the class for a player actor.
	SubType string `json:"subType"`
}

type rawFight struct {
	ID              int32    `json:"id"`
	EncounterID     int32    `json:"encounterID"`
	Name            string   `json:"name"`
	Difficulty      *int32   `json:"difficulty"`
	Size            *int32   `json:"size"`
	Kill            *bool    `json:"kill"`
	BossPercentage  *float64 `json:"bossPercentage"`
	FightPercentage *float64 `json:"fightPercentage"`
	StartTime       float64  `json:"startTime"`
	EndTime         float64  `json:"endTime"`
	InProgress      bool     `json:"inProgress"`
}

// rawTable is the opaque JSON the table query returns. WarcraftLogs documents the shape
// as not frozen, which is exactly why it is decoded here and nowhere else.
type rawTable struct {
	Data struct {
		Entries []rawEntry `json:"entries"`
	} `json:"data"`
}

// rawEntry is one row of a table. DamageDone and Healing report one row per actor with
// a total; Deaths reports one row per death, so a raider who died four times has four
// rows and Total is meaningless on them.
type rawEntry struct {
	ID    int32  `json:"id"`
	Name  string `json:"name"`
	Type  string `json:"type"`
	Total int64  `json:"total"`
}

func (r rawReport) toReport() Report {
	report := Report{
		Code:       r.Code,
		Title:      r.Title,
		StartsAt:   millis(r.StartTime),
		EndsAt:     millis(r.EndTime),
		Revision:   r.Revision,
		Visibility: r.Visibility,
	}
	if r.Zone != nil && r.Zone.Name != "" {
		name := r.Zone.Name
		report.ZoneName = &name
	}
	if r.Region != nil && r.Region.CompactName != "" {
		region := r.Region.CompactName
		report.Region = &region
	}

	report.Fights = make([]Fight, 0, len(r.Fights))
	for _, f := range r.Fights {
		fight := Fight{
			ID:              f.ID,
			EncounterID:     f.EncounterID,
			Name:            f.Name,
			Difficulty:      f.Difficulty,
			Size:            f.Size,
			Kill:            f.Kill != nil && *f.Kill,
			BossPercentage:  f.BossPercentage,
			FightPercentage: f.FightPercentage,
			// Fight times are relative to the report's start. Converting here is what
			// keeps a relative timestamp from ever leaving this package.
			StartsAt:   millis(r.StartTime + f.StartTime),
			EndsAt:     millis(r.StartTime + f.EndTime),
			InProgress: f.InProgress,
		}
		if fight.InProgress {
			report.Live = true
		}
		report.Fights = append(report.Fights, fight)
	}

	report.Raiders = r.raiders()
	return report
}

// raiders folds the three tables onto one row per player.
//
// The tables are the roll call, not masterData: masterData lists everyone the report
// saw at any point, including people who never zoned into a boss pull, and giving them
// a row of zeroes would put somebody on the board who was not in the raid.
func (r rawReport) raiders() []Actor {
	byID := map[int32]*Actor{}

	names := map[int32]rawActor{}
	for _, a := range r.MasterData.Actors {
		names[a.ID] = a
	}

	get := func(id int32, fallbackName, fallbackClass string) *Actor {
		if existing, ok := byID[id]; ok {
			return existing
		}
		actor := &Actor{ID: id, Name: fallbackName, Class: fallbackClass}
		// masterData is where the server lives; the tables do not carry it.
		if known, ok := names[id]; ok {
			actor.Name = known.Name
			actor.Server = known.Server
			if known.SubType != "" {
				actor.Class = known.SubType
			}
		}
		byID[id] = actor
		return actor
	}

	for _, e := range r.Damage.Data.Entries {
		get(e.ID, e.Name, e.Type).Damage = e.Total
	}
	for _, e := range r.Healing.Data.Entries {
		get(e.ID, e.Name, e.Type).Healing = e.Total
	}
	// One row per death, so this counts rows rather than reading a total.
	for _, e := range r.Deaths.Data.Entries {
		get(e.ID, e.Name, e.Type).Deaths++
	}

	actors := make([]Actor, 0, len(byID))
	for _, a := range byID {
		actors = append(actors, *a)
	}
	// Map iteration order is random, and a stored order that changes between two
	// fetches of an unchanged report would make every refetch look like a change.
	sort.Slice(actors, func(i, j int) bool { return actors[i].ID < actors[j].ID })
	return actors
}

func millis(ms float64) time.Time {
	return time.UnixMilli(int64(ms)).UTC()
}
