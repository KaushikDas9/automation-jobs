package usd

import (
	"context"
	"errors"
	"testing"
	"time"

	"gamedeal-radar/daily-check/internal/steam"
)

type fakeRepo struct {
	missing []Game
	saved   map[string]steam.BatchPrice
	failFor string
	listErr error
}

func (f *fakeRepo) ListMissingUSD(context.Context, time.Time) ([]Game, error) {
	return f.missing, f.listErr
}

func (f *fakeRepo) SaveUSDPrice(_ context.Context, id string, p steam.BatchPrice, _ time.Time) error {
	if id == f.failFor {
		return errors.New("write failed")
	}
	if f.saved == nil {
		f.saved = map[string]steam.BatchPrice{}
	}
	f.saved[id] = p
	return nil
}

type fakeFetch struct {
	prices map[string]steam.BatchPrice
	err    error
	gotIDs []string
}

func (f *fakeFetch) FetchBatchPrices(_ context.Context, ids []string, cc string) (map[string]steam.BatchPrice, error) {
	f.gotIDs = ids
	if cc != "us" {
		return nil, errors.New("wrong country")
	}
	return f.prices, f.err
}

func games() []Game {
	return []Game{{ID: "g1", Slug: "a", ExternalAppID: "1"}, {ID: "g2", Slug: "b", ExternalAppID: "2"}, {ID: "g3", Slug: "c", ExternalAppID: "3"}}
}

func TestExecuteSavesAndCounts(t *testing.T) {
	repo := &fakeRepo{missing: games()}
	fetch := &fakeFetch{prices: map[string]steam.BatchPrice{
		"1": {Currency: "USD", Base: 59.99, Current: 17.99, DiscountPct: 70},
		"2": {Unavailable: true},
	}}
	res, err := Execute(context.Background(), repo, fetch, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Missing != 3 || res.Saved != 1 || res.Unavailable != 1 || res.Failed != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if repo.saved["g1"].Current != 17.99 {
		t.Fatalf("g1 not saved: %+v", repo.saved)
	}
	if len(fetch.gotIDs) != 3 {
		t.Fatalf("expected one batch of 3 ids, got %v", fetch.gotIDs)
	}
}

func TestExecuteDryRunWritesNothing(t *testing.T) {
	repo := &fakeRepo{missing: games()[:1]}
	fetch := &fakeFetch{prices: map[string]steam.BatchPrice{"1": {Currency: "USD", Current: 5}}}
	res, err := Execute(context.Background(), repo, fetch, Options{DryRun: true})
	if err != nil || res.Saved != 1 || len(repo.saved) != 0 {
		t.Fatalf("dry run wrote data or miscounted: res=%+v saved=%v err=%v", res, repo.saved, err)
	}
}

func TestExecuteFetchErrorKeepsPartialResults(t *testing.T) {
	repo := &fakeRepo{missing: games()}
	fetch := &fakeFetch{prices: map[string]steam.BatchPrice{"1": {Currency: "USD", Current: 5}}, err: errors.New("429")}
	res, err := Execute(context.Background(), repo, fetch, Options{})
	if err != nil {
		t.Fatalf("a fetch error must not abort the pass: %v", err)
	}
	if res.Saved != 1 || res.Failed != 2 {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestExecuteWriteFailureCounted(t *testing.T) {
	repo := &fakeRepo{missing: games()[:2], failFor: "g2"}
	fetch := &fakeFetch{prices: map[string]steam.BatchPrice{"1": {Currency: "USD", Current: 1}, "2": {Currency: "USD", Current: 2}}}
	res, _ := Execute(context.Background(), repo, fetch, Options{})
	if res.Saved != 1 || res.Failed != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestExecuteLimitAndNothingMissing(t *testing.T) {
	repo := &fakeRepo{missing: games()}
	fetch := &fakeFetch{prices: map[string]steam.BatchPrice{"1": {Currency: "USD", Current: 1}}}
	res, _ := Execute(context.Background(), repo, fetch, Options{Limit: 1})
	if res.Missing != 1 || len(fetch.gotIDs) != 1 {
		t.Fatalf("limit not applied: %+v ids=%v", res, fetch.gotIDs)
	}
	empty, err := Execute(context.Background(), &fakeRepo{}, &fakeFetch{}, Options{})
	if err != nil || empty.Missing != 0 {
		t.Fatalf("empty pass: %+v %v", empty, err)
	}
}

func TestExecuteListErrorAborts(t *testing.T) {
	_, err := Execute(context.Background(), &fakeRepo{listErr: errors.New("db down")}, &fakeFetch{}, Options{})
	if err == nil {
		t.Fatal("expected the listing error to be returned")
	}
}

func TestExecuteRejectsOtherCurrency(t *testing.T) {
	repo := &fakeRepo{missing: games()[:1]}
	fetch := &fakeFetch{prices: map[string]steam.BatchPrice{"1": {Currency: "INR", Current: 500}}}
	res, _ := Execute(context.Background(), repo, fetch, Options{})
	if res.Saved != 0 || res.Failed != 1 || len(repo.saved) != 0 {
		t.Fatalf("a non-USD price must not be saved: %+v", res)
	}
}
