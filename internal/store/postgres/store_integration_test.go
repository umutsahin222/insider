//go:build integration

package postgres

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/umutsahin222/insider/internal/domain"
)

var testStore *Store

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	ctx := context.Background()
	ctr, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("notify"), tcpostgres.WithUsername("notify"), tcpostgres.WithPassword("notify"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).WithStartupTimeout(60*time.Second)))
	if err != nil {
		log.Printf("starting postgres: %v", err)
		return 1
	}
	defer func() { _ = testcontainers.TerminateContainer(ctr) }()

	url, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		log.Print(err)
		return 1
	}
	if err := Migrate(url); err != nil {
		log.Print(err)
		return 1
	}
	pool, err := Connect(ctx, url)
	if err != nil {
		log.Print(err)
		return 1
	}
	defer pool.Close()
	testStore = New(pool)
	return m.Run()
}

func newNotification(t *testing.T, mutate func(*domain.CreateInput)) domain.Notification {
	t.Helper()
	in := domain.CreateInput{Channel: "sms", Recipient: "+905551234567", Content: "hello", CorrelationID: "corr"}
	if mutate != nil {
		mutate(&in)
	}
	n, err := domain.NewNotification(in, domain.DefaultRules(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCreateAndGet(t *testing.T) {
	ctx := context.Background()
	scheduled := time.Now().Add(time.Hour)
	n := newNotification(t, func(in *domain.CreateInput) { in.Priority = "high"; in.ScheduledAt = &scheduled })

	created, isNew, err := testStore.CreateNotification(ctx, n)
	if err != nil || !isNew {
		t.Fatalf("create: isNew=%v err=%v", isNew, err)
	}
	got, err := testStore.GetNotification(ctx, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != created.ID || got.Status != domain.StatusPending || got.Priority != domain.PriorityHigh ||
		got.Channel != domain.ChannelSMS || got.CorrelationID != "corr" || got.MaxAttempts != 5 {
		t.Errorf("round trip mismatch: %+v", got)
	}
	if got.RequestHash == "" || got.RequestHash != n.RequestHash {
		t.Errorf("request_hash = %q, want %q", got.RequestHash, n.RequestHash)
	}
	if got.ScheduledAt == nil || !got.NextAttemptAt.After(time.Now()) {
		t.Errorf("schedule not persisted: scheduled=%v next=%v", got.ScheduledAt, got.NextAttemptAt)
	}

	if _, err := testStore.GetNotification(ctx, uuid.New()); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("missing id: err = %v, want ErrNotFound", err)
	}
}

func TestCreate_IdempotencyIsRaceFree(t *testing.T) {
	ctx := context.Background()
	key := "idem-" + uuid.NewString()

	const workers = 20
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		created int
		ids     = map[uuid.UUID]bool{}
	)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := newNotification(t, func(in *domain.CreateInput) { in.IdempotencyKey = key })
			got, isNew, err := testStore.CreateNotification(ctx, n)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if isNew {
				created++
			}
			ids[got.ID] = true
		}()
	}
	wg.Wait()
	if created != 1 || len(ids) != 1 {
		t.Errorf("created=%d distinct ids=%d, want exactly one notification", created, len(ids))
	}
}

func TestCreateBatch(t *testing.T) {
	ctx := context.Background()
	items := make([]domain.CreateInput, 1000)
	for i := range items {
		items[i] = domain.CreateInput{Channel: "email", Recipient: fmt.Sprintf("u%d@example.com", i), Content: "hi",
			IdempotencyKey: fmt.Sprintf("batch-%s-%d", t.Name(), i)}
	}
	batch, err := domain.NewBatch(items, "batch-key-"+uuid.NewString(), domain.DefaultRules(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	created, isNew, err := testStore.CreateBatch(ctx, batch)
	if err != nil || !isNew || created.ID != batch.ID {
		t.Fatalf("create: isNew=%v id=%v err=%v", isNew, created.ID, err)
	}
	sum, err := testStore.BatchSummary(ctx, batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Total != 1000 || sum.Counts[domain.StatusPending] != 1000 {
		t.Errorf("summary = %+v", sum)
	}

	t.Run("replay returns the original batch in request order", func(t *testing.T) {
		// A retried request is rebuilt from scratch: new notification IDs, new batch ID.
		retry, err := domain.NewBatch(items, *batch.IdempotencyKey, domain.DefaultRules(), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		got, isNew, err := testStore.CreateBatch(ctx, retry)
		if err != nil || isNew {
			t.Fatalf("replay: isNew=%v err=%v", isNew, err)
		}
		if got.ID != batch.ID || got.RequestHash != batch.RequestHash || len(got.Notifications) != len(batch.Notifications) {
			t.Fatalf("replay = id %v hash %q n=%d", got.ID, got.RequestHash, len(got.Notifications))
		}
		for i := range got.Notifications {
			if got.Notifications[i].ID != batch.Notifications[i].ID {
				t.Fatalf("notification %d: %v, want %v (order not preserved)", i, got.Notifications[i].ID, batch.Notifications[i].ID)
			}
		}
		if sum, _ := testStore.BatchSummary(ctx, batch.ID); sum.Total != 1000 {
			t.Errorf("replay inserted rows: total = %d", sum.Total)
		}
	})

	t.Run("reused item idempotency key rejects the whole batch", func(t *testing.T) {
		again := []domain.CreateInput{
			{Channel: "sms", Recipient: "+905551234567", Content: "new", IdempotencyKey: "fresh-" + uuid.NewString()},
			{Channel: "sms", Recipient: "+905551234567", Content: "dup", IdempotencyKey: items[0].IdempotencyKey},
		}
		b2, err := domain.NewBatch(again, "batch-key-"+uuid.NewString(), domain.DefaultRules(), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := testStore.CreateBatch(ctx, b2); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("err = %v, want ErrConflict", err)
		}
		if _, err := testStore.BatchSummary(ctx, b2.ID); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("partial batch persisted: %v", err)
		}
		var batchRows int
		if err := testStore.pool.QueryRow(ctx, `SELECT count(*) FROM batches WHERE id = $1`, b2.ID).Scan(&batchRows); err != nil || batchRows != 0 {
			t.Errorf("batch row persisted after rollback: rows=%d err=%v", batchRows, err)
		}
	})
}

func TestCreateBatch_IdempotencyIsRaceFree(t *testing.T) {
	ctx := context.Background()
	key := "batch-race-" + uuid.NewString()
	items := []domain.CreateInput{
		{Channel: "sms", Recipient: "+905551234567", Content: "a"},
		{Channel: "push", Recipient: "tok", Content: "b"},
	}

	const workers = 20
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		created int
		ids     = map[uuid.UUID]bool{}
	)
	for range workers {
		wg.Go(func() {
			b, err := domain.NewBatch(items, key, domain.DefaultRules(), time.Now())
			if err != nil {
				t.Error(err)
				return
			}
			got, isNew, err := testStore.CreateBatch(ctx, b)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if isNew {
				created++
			}
			ids[got.ID] = true
		})
	}
	wg.Wait()
	if created != 1 || len(ids) != 1 {
		t.Errorf("created=%d distinct batch ids=%d, want exactly one batch", created, len(ids))
	}
}

func TestCancel(t *testing.T) {
	ctx := context.Background()
	n := newNotification(t, nil)
	if _, _, err := testStore.CreateNotification(ctx, n); err != nil {
		t.Fatal(err)
	}

	got, err := testStore.CancelNotification(ctx, n.ID)
	if err != nil || got.Status != domain.StatusCancelled {
		t.Fatalf("cancel: status=%v err=%v", got.Status, err)
	}
	if _, err := testStore.CancelNotification(ctx, n.ID); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("second cancel: err = %v, want ErrConflict", err)
	}
	if _, err := testStore.CancelNotification(ctx, uuid.New()); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown id: err = %v, want ErrNotFound", err)
	}
}

func TestCancelBatch(t *testing.T) {
	ctx := context.Background()
	batch, err := domain.NewBatch([]domain.CreateInput{
		{Channel: "push", Recipient: "tok-1", Content: "a"},
		{Channel: "push", Recipient: "tok-2", Content: "b"},
	}, "", domain.DefaultRules(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := testStore.CreateBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	batchID, ns := batch.ID, batch.Notifications
	if _, err := testStore.CancelNotification(ctx, ns[0].ID); err != nil {
		t.Fatal(err)
	}

	n, err := testStore.CancelBatch(ctx, batchID)
	if err != nil || n != 1 {
		t.Fatalf("cancelled=%d err=%v, want 1", n, err)
	}
	if n, err := testStore.CancelBatch(ctx, batchID); err != nil || n != 0 {
		t.Errorf("repeat: cancelled=%d err=%v, want 0", n, err)
	}
	if _, err := testStore.CancelBatch(ctx, uuid.New()); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown batch: err = %v", err)
	}
}

func TestListNotifications_KeysetPagination(t *testing.T) {
	ctx := context.Background()
	items := make([]domain.CreateInput, 7)
	for i := range items {
		ch := "sms"
		if i%2 == 1 {
			ch = "push"
		}
		items[i] = domain.CreateInput{Channel: ch, Recipient: "+905551234567", Content: "x"}
		if ch == "push" {
			items[i].Recipient = "tok"
		}
	}
	batch, err := domain.NewBatch(items, "", domain.DefaultRules(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := testStore.CreateBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	batchID := batch.ID

	// Page through the batch 3 at a time; every row must appear exactly once.
	seen := map[uuid.UUID]bool{}
	filter := domain.ListFilter{BatchID: &batchID, Limit: 3}
	pages := 0
	for {
		page, err := testStore.ListNotifications(ctx, filter)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, n := range page.Items {
			if seen[n.ID] {
				t.Fatalf("duplicate %s across pages", n.ID)
			}
			seen[n.ID] = true
		}
		if page.Next == nil {
			break
		}
		filter.After = page.Next
	}
	if len(seen) != 7 || pages != 3 {
		t.Errorf("seen=%d pages=%d, want 7 rows in 3 pages", len(seen), pages)
	}

	push := domain.ChannelPush
	page, err := testStore.ListNotifications(ctx, domain.ListFilter{BatchID: &batchID, Channel: &push, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 3 {
		t.Errorf("push filter: got %d, want 3", len(page.Items))
	}

	future := time.Now().Add(time.Hour)
	page, err = testStore.ListNotifications(ctx, domain.ListFilter{BatchID: &batchID, From: &future, Limit: 50})
	if err != nil || len(page.Items) != 0 {
		t.Errorf("from filter: got %d rows, err %v", len(page.Items), err)
	}
}
