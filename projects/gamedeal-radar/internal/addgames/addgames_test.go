package addgames

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"gamedeal-radar/daily-check/internal/steam"
	"gamedeal-radar/daily-check/internal/storedetails"
)

type fakeQueue struct {
	reqs      []Request
	claimErr  error
	finished  map[string][2]string // id -> status, note
	activated []string
	actErr    error
	claimDry  bool
	claimMax  int
	claimIDs  []string
}

func (q *fakeQueue) Claim(_ context.Context, max int, ids []string, dry bool) ([]Request, error) {
	q.claimMax, q.claimIDs, q.claimDry = max, ids, dry
	return q.reqs, q.claimErr
}

func (q *fakeQueue) Finish(_ context.Context, id, status, note string) error {
	if q.finished == nil {
		q.finished = map[string][2]string{}
	}
	q.finished[id] = [2]string{status, note}
	return nil
}

func (q *fakeQueue) Activate(_ context.Context, gameID string) error {
	if q.actErr != nil {
		return q.actErr
	}
	q.activated = append(q.activated, gameID)
	return nil
}

// fakeSteps returns canned results by step name and records the calls.
type fakeSteps struct {
	res         map[string]StepResult
	calls       []string
	cancelAfter func()
}

func (f *fakeSteps) do(name string) StepResult {
	f.calls = append(f.calls, name)
	if r, ok := f.res[name]; ok {
		return r
	}
	return OK(name)
}
func (f *fakeSteps) Details(context.Context, Request) StepResult { return f.do("details") }

// EpicDetails and GOGDetails are recorded as "epic-details" and "gog-details"; their canned
// results default to OK("details"), the name the real steps report.
func (f *fakeSteps) EpicDetails(context.Context, Request) StepResult {
	return f.doAs("epic-details", "details")
}
func (f *fakeSteps) GOGDetails(context.Context, Request) StepResult {
	return f.doAs("gog-details", "details")
}

func (f *fakeSteps) doAs(call, name string) StepResult {
	f.calls = append(f.calls, call)
	if r, ok := f.res[call]; ok {
		return r
	}
	return OK(name)
}
func (f *fakeSteps) INR(context.Context, Request) StepResult  { return f.do("inr") }
func (f *fakeSteps) USD(context.Context, Request) StepResult  { return f.do("usd") }
func (f *fakeSteps) Tags(context.Context, Request) StepResult { return f.do("tags") }
func (f *fakeSteps) GOG(context.Context, Request) StepResult  { return f.do("gog") }
func (f *fakeSteps) Epic(context.Context, Request) StepResult {
	r := f.do("epic")
	if f.cancelAfter != nil {
		f.cancelAfter()
	}
	return r
}

func req(id string) Request {
	return Request{ID: id, StoreCode: StoreSteam, ExternalID: "620", SteamAppID: "620", Name: "Game " + id, GameID: "g-" + id}
}

func epicReq(id, slug string) Request {
	return Request{ID: id, StoreCode: StoreEpic, ExternalID: slug, Name: "Epic " + id, GameID: "g-" + id}
}

func gogReq(id, slug string) Request {
	return Request{ID: id, StoreCode: StoreGOG, ExternalID: slug, Name: "GOG " + id, GameID: "g-" + id}
}

func TestAllStepsOKActivatesAndDone(t *testing.T) {
	q := &fakeQueue{reqs: []Request{req("1")}}
	st := &fakeSteps{}
	sum, err := Run(context.Background(), q, st, Options{Max: 10})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Claimed != 1 || sum.Done != 1 || sum.Failed != 0 {
		t.Fatalf("summary = %+v", sum)
	}
	if got := q.finished["1"]; got != [2]string{StatusDone, ""} {
		t.Fatalf("finish = %v", got)
	}
	if len(q.activated) != 1 || q.activated[0] != "g-1" {
		t.Fatalf("activated = %v", q.activated)
	}
	if q.claimMax != 10 || q.claimDry {
		t.Fatalf("claim args max=%d dry=%t", q.claimMax, q.claimDry)
	}
}

func TestEmptyQueue(t *testing.T) {
	q := &fakeQueue{}
	sum, err := Run(context.Background(), q, &fakeSteps{}, Options{Max: 10})
	if err != nil || sum.Claimed != 0 || sum.AllFailed() {
		t.Fatalf("empty queue: %+v %v", sum, err)
	}
	if len(q.finished) != 0 {
		t.Fatal("nothing should be finished")
	}
}

func TestClaimErrorIsReturned(t *testing.T) {
	q := &fakeQueue{claimErr: errors.New("boom")}
	if _, err := Run(context.Background(), q, &fakeSteps{}, Options{Max: 1}); err == nil {
		t.Fatal("claim error must be returned")
	}
}

func TestOptionalFailuresNeverBlockActivation(t *testing.T) {
	q := &fakeQueue{reqs: []Request{req("1")}}
	st := &fakeSteps{res: map[string]StepResult{
		"usd":  Failed("usd", "fetch or save failed", errors.New("secret https://x?key=abc")),
		"tags": Skipped("tags", "no tags"),
		"gog":  Skipped("gog", "no match"),
		"epic": Skipped("epic", "skipped, no api key"),
	}}
	sum, _ := Run(context.Background(), q, st, Options{Max: 10})
	if sum.Done != 1 || len(q.activated) != 1 {
		t.Fatalf("optional failures must not block: %+v", sum)
	}
	got := q.finished["1"]
	if got[0] != StatusDone {
		t.Fatalf("status = %s", got[0])
	}
	for _, want := range []string{"usd: failed (fetch or save failed)", "tags: no tags", "gog: no match", "epic: skipped, no api key"} {
		if !strings.Contains(got[1], want) {
			t.Errorf("note %q lacks %q", got[1], want)
		}
	}
	if strings.Contains(got[1], "secret") || strings.Contains(got[1], "key=") {
		t.Errorf("note leaks error text: %q", got[1])
	}
}

func TestCoreFailureLeavesGameInactiveAndSkipsOptional(t *testing.T) {
	for _, core := range []string{"details", "inr"} {
		q := &fakeQueue{reqs: []Request{req("1")}}
		st := &fakeSteps{res: map[string]StepResult{
			core: Failed(core, "steam temporarily unavailable", errors.New("GET https://store.steampowered.com/x: timeout")),
		}}
		sum, _ := Run(context.Background(), q, st, Options{Max: 10})
		if sum.Failed != 1 || !sum.AllFailed() {
			t.Fatalf("%s: summary %+v", core, sum)
		}
		if len(q.activated) != 0 {
			t.Fatalf("%s: game must stay inactive", core)
		}
		got := q.finished["1"]
		if got[0] != StatusFailed || !strings.Contains(got[1], core+": steam temporarily unavailable") {
			t.Fatalf("%s: finish = %v", core, got)
		}
		if strings.Contains(got[1], "http") {
			t.Fatalf("note leaks a URL: %q", got[1])
		}
		for _, c := range st.calls {
			if c == "usd" || c == "gog" || c == "tags" || c == "epic" {
				t.Fatalf("%s: optional step %s ran after a core failure", core, c)
			}
		}
	}
}

func TestDetailsFailureStillRunsINR(t *testing.T) {
	q := &fakeQueue{reqs: []Request{req("1")}}
	st := &fakeSteps{res: map[string]StepResult{"details": Failed("details", "x", errors.New("e"))}}
	Run(context.Background(), q, st, Options{Max: 1})
	if len(st.calls) != 2 || st.calls[1] != "inr" {
		t.Fatalf("calls = %v", st.calls)
	}
}

func TestINRSkippedStillActivates(t *testing.T) {
	q := &fakeQueue{reqs: []Request{req("1")}}
	st := &fakeSteps{res: map[string]StepResult{"inr": Skipped("inr", "no steam price in India")}}
	sum, _ := Run(context.Background(), q, st, Options{Max: 1})
	if sum.Done != 1 || len(q.activated) != 1 {
		t.Fatalf("a game without an INR price is still activated: %+v", sum)
	}
	if !strings.Contains(q.finished["1"][1], "inr: no steam price in India") {
		t.Fatalf("note = %q", q.finished["1"][1])
	}
}

func TestNotFound(t *testing.T) {
	q := &fakeQueue{reqs: []Request{req("1")}}
	st := &fakeSteps{res: map[string]StepResult{
		"details": Failed("details", "steam has no such app", fmt.Errorf("wrapped: %w", steam.ErrNotFound)),
	}}
	sum, _ := Run(context.Background(), q, st, Options{Max: 1})
	if sum.NotFound != 1 || sum.AllFailed() {
		t.Fatalf("summary %+v", sum)
	}
	if got := q.finished["1"]; got[0] != StatusNotFound || got[1] != "steam has no such app" {
		t.Fatalf("finish = %v", got)
	}
	if len(q.activated) != 0 || len(st.calls) != 1 {
		t.Fatalf("game stays inactive and later steps are skipped: calls=%v", st.calls)
	}
}

func TestInvalidAppIDIsNotFoundWithoutFetching(t *testing.T) {
	r := req("1")
	r.SteamAppID = "12; DROP"
	q := &fakeQueue{reqs: []Request{r}}
	st := &fakeSteps{}
	sum, _ := Run(context.Background(), q, st, Options{Max: 1})
	if sum.NotFound != 1 || len(st.calls) != 0 {
		t.Fatalf("summary %+v calls %v", sum, st.calls)
	}
}

func TestActivateFailureIsFailed(t *testing.T) {
	q := &fakeQueue{reqs: []Request{req("1")}, actErr: errors.New("permission denied for table games")}
	sum, _ := Run(context.Background(), q, &fakeSteps{}, Options{Max: 1})
	if sum.Failed != 1 {
		t.Fatalf("summary %+v", sum)
	}
	got := q.finished["1"]
	if got[0] != StatusFailed || strings.Contains(got[1], "permission") {
		t.Fatalf("finish = %v", got)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	q := &fakeQueue{reqs: []Request{req("1"), req("2")}}
	st := &fakeSteps{}
	sum, _ := Run(context.Background(), q, st, Options{Max: 10, DryRun: true})
	if !q.claimDry {
		t.Fatal("dry run must claim read-only")
	}
	if sum.Done != 2 {
		t.Fatalf("summary %+v", sum)
	}
	if len(q.finished) != 0 || len(q.activated) != 0 {
		t.Fatalf("dry run changed state: finished=%v activated=%v", q.finished, q.activated)
	}
}

func TestMixedRunStatusCounts(t *testing.T) {
	q := &fakeQueue{reqs: []Request{req("1"), req("2"), req("3")}}
	st := &perGameSteps{fail: map[string]bool{"g-2": true}}
	sum, _ := Run(context.Background(), q, st, Options{Max: 10})
	if sum.Done != 2 || sum.Failed != 1 || sum.AllFailed() {
		t.Fatalf("summary %+v", sum)
	}
	if q.finished["2"][0] != StatusFailed || q.finished["3"][0] != StatusDone {
		t.Fatalf("finished = %v", q.finished)
	}
}

type perGameSteps struct{ fail map[string]bool }

func (p *perGameSteps) Details(_ context.Context, r Request) StepResult {
	if p.fail[r.GameID] {
		return Failed("details", "steam request failed", errors.New("x"))
	}
	return OK("details")
}
func (p *perGameSteps) EpicDetails(context.Context, Request) StepResult { return OK("details") }
func (p *perGameSteps) GOGDetails(context.Context, Request) StepResult  { return OK("details") }
func (p *perGameSteps) INR(context.Context, Request) StepResult         { return OK("inr") }
func (p *perGameSteps) USD(context.Context, Request) StepResult         { return OK("usd") }
func (p *perGameSteps) Tags(context.Context, Request) StepResult        { return OK("tags") }
func (p *perGameSteps) GOG(context.Context, Request) StepResult         { return OK("gog") }
func (p *perGameSteps) Epic(context.Context, Request) StepResult        { return OK("epic") }

func TestCancelledRunMarksRemainingFailedForRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	q := &fakeQueue{reqs: []Request{req("1"), req("2")}}
	st := &fakeSteps{cancelAfter: cancel}
	sum, _ := Run(ctx, q, st, Options{Max: 10})
	if sum.Done != 1 || sum.Failed != 1 {
		t.Fatalf("summary %+v", sum)
	}
	if got := q.finished["2"]; got[0] != StatusFailed || !strings.Contains(got[1], "cut short") {
		t.Fatalf("unprocessed request must go back to failed: %v", got)
	}
	if len(q.finished) != 2 {
		t.Fatal("every claimed row must be finished, none left in processing")
	}
}

func TestNoteIsClipped(t *testing.T) {
	long := strings.Repeat("x", 500)
	if got := clip(long); len(got) != maxNote {
		t.Fatalf("len = %d", len(got))
	}
}

func TestStepsLine(t *testing.T) {
	got := StepsLine([]StepResult{OK("details"), Skipped("epic", "n"), Failed("gog", "n", nil)})
	if got != "details=ok epic=skipped gog=failed" {
		t.Fatalf("line = %q", got)
	}
}

func hasCall(calls []string, name string) bool {
	for _, c := range calls {
		if c == name {
			return true
		}
	}
	return false
}

func TestEpicRequestRunsStorePageThenPrices(t *testing.T) {
	q := &fakeQueue{reqs: []Request{epicReq("1", "alan-wake-2")}}
	st := &fakeSteps{res: map[string]StepResult{"epic": Skipped("epic", "skipped, no api key"), "gog": Skipped("gog", "no match")}}
	sum, _ := Run(context.Background(), q, st, Options{Max: 10})
	if sum.Done != 1 || len(q.activated) != 1 || q.activated[0] != "g-1" {
		t.Fatalf("summary %+v activated %v", sum, q.activated)
	}
	want := []string{"epic-details", "epic", "gog"}
	if strings.Join(st.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", st.calls, want)
	}
	got := q.finished["1"]
	if got[0] != StatusDone || got[1] != "epic: skipped, no api key; gog: no match" {
		t.Fatalf("finish = %v", got)
	}
}

func TestGOGRequestRunsStorePageThenPrices(t *testing.T) {
	q := &fakeQueue{reqs: []Request{gogReq("1", "alan_wake")}}
	st := &fakeSteps{}
	sum, _ := Run(context.Background(), q, st, Options{Max: 10})
	if sum.Done != 1 || len(q.activated) != 1 {
		t.Fatalf("summary %+v", sum)
	}
	if strings.Join(st.calls, ",") != "gog-details,gog,epic" {
		t.Fatalf("calls = %v", st.calls)
	}
	if q.finished["1"] != [2]string{StatusDone, ""} {
		t.Fatalf("finish = %v", q.finished["1"])
	}
}

func TestNonSteamRequestsNeverRunSteamSteps(t *testing.T) {
	for _, r := range []Request{epicReq("1", "alan-wake-2"), gogReq("2", "alan_wake")} {
		// A Steam app id on the request (the game is on Steam too) changes nothing.
		r.SteamAppID = "108710"
		st := &fakeSteps{}
		out := Process(context.Background(), &fakeQueue{}, st, r, false)
		if out.Status != StatusDone {
			t.Fatalf("%s: %+v", r.StoreCode, out)
		}
		for _, c := range []string{"details", "inr", "usd", "tags"} {
			if hasCall(st.calls, c) {
				t.Fatalf("%s request ran Steam step %s: %v", r.StoreCode, c, st.calls)
			}
		}
	}
}

func TestStorePageFailureIsFailedAndSkipsPrices(t *testing.T) {
	for _, r := range []Request{epicReq("1", "alan-wake-2"), gogReq("1", "alan_wake")} {
		call := r.StoreCode + "-details"
		q := &fakeQueue{reqs: []Request{r}}
		st := &fakeSteps{res: map[string]StepResult{
			call: Failed("details", r.StoreCode+" temporarily unavailable", errors.New("GET https://x: HTTP 503")),
		}}
		sum, _ := Run(context.Background(), q, st, Options{Max: 1})
		if sum.Failed != 1 || len(q.activated) != 0 {
			t.Fatalf("%s: summary %+v", r.StoreCode, sum)
		}
		got := q.finished["1"]
		if got[0] != StatusFailed || got[1] != "details: "+r.StoreCode+" temporarily unavailable" {
			t.Fatalf("%s: finish = %v", r.StoreCode, got)
		}
		if len(st.calls) != 1 {
			t.Fatalf("%s: price steps ran after a core failure: %v", r.StoreCode, st.calls)
		}
	}
}

func TestStorePageNotFound(t *testing.T) {
	for _, r := range []Request{epicReq("1", "no-such-game"), gogReq("1", "no_such_game")} {
		call := r.StoreCode + "-details"
		q := &fakeQueue{reqs: []Request{r}}
		st := &fakeSteps{res: map[string]StepResult{
			call: Failed("details", r.StoreCode+" has no such product", fmt.Errorf("x: %w", storedetails.ErrNotFound)),
		}}
		sum, _ := Run(context.Background(), q, st, Options{Max: 1})
		if sum.NotFound != 1 || len(q.activated) != 0 || len(st.calls) != 1 {
			t.Fatalf("%s: summary %+v calls %v", r.StoreCode, sum, st.calls)
		}
		if got := q.finished["1"]; got != [2]string{StatusNotFound, r.StoreCode + " has no such product"} {
			t.Fatalf("%s: finish = %v", r.StoreCode, got)
		}
	}
}

func TestStorePagePriceFailuresNeverBlock(t *testing.T) {
	q := &fakeQueue{reqs: []Request{epicReq("1", "alan-wake-2")}}
	st := &fakeSteps{res: map[string]StepResult{
		"epic": Failed("epic", "blocked by the store", errors.New("api key=abc")),
		"gog":  Failed("gog", "lookup failed", nil),
	}}
	sum, _ := Run(context.Background(), q, st, Options{Max: 1})
	if sum.Done != 1 || len(q.activated) != 1 {
		t.Fatalf("summary %+v", sum)
	}
	if got := q.finished["1"][1]; got != "epic: failed (blocked by the store); gog: failed (lookup failed)" {
		t.Fatalf("note = %q", got)
	}
}

func TestInvalidStoreIDsAreNotFoundWithoutFetching(t *testing.T) {
	for _, r := range []Request{
		epicReq("1", "../admin"), epicReq("2", "Alan-Wake-2"), epicReq("3", "https://evil.example/x"),
		epicReq("4", strings.Repeat("a", 121)), gogReq("5", "../x"), gogReq("6", "Alan_Wake"), epicReq("7", ""),
	} {
		st := &fakeSteps{}
		out := Process(context.Background(), &fakeQueue{}, st, r, false)
		if out.Status != StatusNotFound || len(st.calls) != 0 || !strings.HasPrefix(out.Note, "invalid ") {
			t.Errorf("%s %q: %+v calls %v", r.StoreCode, r.ExternalID, out, st.calls)
		}
		if strings.Contains(out.Note, r.ExternalID) && r.ExternalID != "" {
			t.Errorf("note echoes the id: %q", out.Note)
		}
	}
}

func TestUnknownStoreFails(t *testing.T) {
	r := req("1")
	r.StoreCode = "itch"
	st := &fakeSteps{}
	out := Process(context.Background(), &fakeQueue{}, st, r, false)
	if out.Status != StatusFailed || len(st.calls) != 0 {
		t.Fatalf("%+v %v", out, st.calls)
	}
}

func TestEmptyStoreCodeIsSteam(t *testing.T) {
	r := req("1")
	r.StoreCode = ""
	st := &fakeSteps{}
	out := Process(context.Background(), &fakeQueue{}, st, r, true)
	if out.Status != StatusDone || !hasCall(st.calls, "inr") {
		t.Fatalf("%+v %v", out, st.calls)
	}
}

func TestStoreRequestDryRunWritesNothing(t *testing.T) {
	q := &fakeQueue{reqs: []Request{epicReq("1", "alan-wake-2"), gogReq("2", "alan_wake")}}
	st := &fakeSteps{}
	sum, _ := Run(context.Background(), q, st, Options{Max: 10, DryRun: true})
	if !q.claimDry || sum.Done != 2 || len(q.finished) != 0 || len(q.activated) != 0 {
		t.Fatalf("summary %+v finished %v activated %v", sum, q.finished, q.activated)
	}
}

func TestStoreActivateFailureIsFailed(t *testing.T) {
	q := &fakeQueue{reqs: []Request{epicReq("1", "alan-wake-2")}, actErr: errors.New("permission denied")}
	sum, _ := Run(context.Background(), q, &fakeSteps{}, Options{Max: 1})
	if sum.Failed != 1 || q.finished["1"] != [2]string{StatusFailed, "activate: database update failed"} {
		t.Fatalf("summary %+v finish %v", sum, q.finished["1"])
	}
}
