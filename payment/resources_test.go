package payment

import (
	"context"
	"encoding/json"
	"errors"
	"one-api/model"
	"one-api/payment/types"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const lifecycleKind = "lifecycle-fixture"

type lifecycleFactory struct {
	mu        sync.Mutex
	clients   []*lifecycleClient
	gate      <-chan struct{}
	setupFail bool
	onCreate  func(types.GatewaySnapshot)
}

func (*lifecycleFactory) Descriptor() types.GatewayDescriptor {
	return types.GatewayDescriptor{Kind: lifecycleKind, Products: []string{"lifecycle.checkout"}}
}
func (*lifecycleFactory) ValidateConfig(_ context.Context, input types.GatewayConfigInput) (types.GatewayBinding, error) {
	var config map[string]string
	if err := json.Unmarshal([]byte(input.Config), &config); err != nil {
		return types.GatewayBinding{}, err
	}
	identity := types.GatewayIdentity{Kind: lifecycleKind, Environment: "test", MerchantAccount: config["merchant"], ProtocolProfile: "v1"}
	return types.GatewayBinding{Identity: identity, TransactionNamespace: types.Namespace(lifecycleKind, config["merchant"]), DefaultProduct: "lifecycle.checkout", Config: input.Config}, nil
}
func (f *lifecycleFactory) NewClient(ctx context.Context, snapshot types.GatewaySnapshot) (GatewayClient, error) {
	client := &lifecycleClient{ctx: ctx, snapshot: snapshot, done: make(chan struct{}), setupFail: f.setupFail}
	f.mu.Lock()
	f.clients = append(f.clients, client)
	f.mu.Unlock()
	go func() {
		<-ctx.Done()
		if f.gate != nil {
			<-f.gate
		}
		close(client.done)
	}()
	if f.onCreate != nil {
		f.onCreate(snapshot)
	}
	return client, nil
}

type lifecycleClient struct {
	ctx       context.Context
	snapshot  types.GatewaySnapshot
	done      chan struct{}
	once      sync.Once
	stops     atomic.Int32
	setupFail bool
}

func (*lifecycleClient) Capabilities(string) (types.Capabilities, error) {
	return types.Capabilities{Currencies: []string{"CNY"}, PreparationMode: types.BrowserHandoff, CanReceiveCallbacks: true, CanConfigureCallbacks: true, DisplayWindow: time.Hour, ActionKinds: []string{"redirect"}}, nil
}
func (*lifecycleClient) PreparePayment(context.Context, types.FrozenOrder) (types.PrepareResult, error) {
	return types.PrepareResult{}, types.ErrUnsupported
}
func (*lifecycleClient) VerifyNotification(context.Context, types.CallbackRequest) (types.NotificationResult, error) {
	return types.NotificationResult{Ignored: true}, nil
}
func (*lifecycleClient) CallbackResponse(types.CallbackOutcome) types.CallbackResponse {
	return types.CallbackResponse{StatusCode: 200}
}
func (c *lifecycleClient) ConfigureCallbacks(context.Context, types.CallbackEndpoint) (types.SetupResult, error) {
	if c.setupFail {
		return types.SetupResult{}, errors.New("fixture setup failed")
	}
	return types.SetupResult{Ready: true, Config: c.snapshot.Config}, nil
}
func (c *lifecycleClient) CloseResources() error {
	c.once.Do(func() { <-c.done; c.stops.Add(1) })
	return nil
}
func lifecycleSnapshot(revision int64) types.GatewaySnapshot {
	return types.GatewaySnapshot{GatewayID: 700, CredentialRevision: revision, Identity: types.GatewayIdentity{Kind: lifecycleKind, Environment: "test", MerchantAccount: "merchant", ProtocolProfile: "v1"}, TransactionNamespace: types.Namespace(lifecycleKind, "merchant"), DefaultProduct: "lifecycle.checkout", Config: `{"merchant":"merchant","key":"A"}`}
}
func TestResourcesReuseAndRetireAfterLastHandleAndWorker(t *testing.T) {
	gate := make(chan struct{})
	factory := &lifecycleFactory{gate: gate}
	RegisterFactory(factory)
	pool := NewResourcePool(context.Background())
	first, err := pool.Acquire(context.Background(), lifecycleSnapshot(1))
	if err != nil {
		t.Fatal(err)
	}
	same, err := pool.Acquire(context.Background(), lifecycleSnapshot(1))
	if err != nil || same.Client != first.Client || len(factory.clients) != 1 {
		t.Fatal("same revision initialized twice")
	}
	second, err := pool.Acquire(context.Background(), lifecycleSnapshot(2))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Acquire(context.Background(), lifecycleSnapshot(1)); err == nil {
		t.Fatal("stale snapshot reopened retired revision")
	}
	old := first.Client.(*lifecycleClient)
	first.Release()
	if old.ctx.Err() != nil {
		t.Fatal("retired active holder was canceled")
	}
	released := make(chan struct{})
	go func() { same.Release(); close(released) }()
	select {
	case <-old.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("retirement did not cancel worker")
	}
	select {
	case <-released:
		t.Fatal("retirement returned before worker stopped")
	default:
	}
	close(gate)
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("retirement stuck")
	}
	if old.stops.Load() != 1 {
		t.Fatal("worker not stopped once")
	}
	second.Release()
	pool.Close()
	if factory.clients[1].stops.Load() != 1 {
		t.Fatal("service close did not retire live revision")
	}
}
func TestResourcesShutdownWaitsForDrainingRevision(t *testing.T) {
	factory := &lifecycleFactory{}
	RegisterFactory(factory)
	pool := NewResourcePool(context.Background())
	old, _ := pool.Acquire(context.Background(), lifecycleSnapshot(1))
	latest, _ := pool.Acquire(context.Background(), lifecycleSnapshot(2))
	latest.Release()
	shutdown := make(chan struct{})
	go func() { pool.Close(); close(shutdown) }()
	select {
	case <-shutdown:
		t.Fatal("shutdown ignored old in-flight resource")
	case <-time.After(10 * time.Millisecond):
	}
	if old.Client.(*lifecycleClient).ctx.Err() != nil {
		t.Fatal("shutdown canceled in-flight old revision")
	}
	old.Release()
	select {
	case <-shutdown:
	case <-time.After(time.Second):
		t.Fatal("shutdown stuck")
	}
}
func TestGatewaySetupFailureCleansUnpublishedCandidate(t *testing.T) {
	newServiceFixture(t, "callback")
	factory := &lifecycleFactory{setupFail: true}
	RegisterFactory(factory)
	p := &model.Payment{Type: lifecycleKind, Name: "测试", Currency: model.CurrencyTypeCNY, Config: `{"merchant":"merchant","key":"A"}`}
	if err := CreateGateway(context.Background(), p); err == nil {
		t.Fatal("setup unexpectedly succeeded")
	}
	if p.ID == 0 || len(factory.clients) != 1 || factory.clients[0].stops.Load() != 1 {
		t.Fatal("failed candidate not persisted/cleaned")
	}
	persisted, err := model.GetPaymentByID(p.ID)
	if err != nil || persisted.SetupStatus != "failed" {
		t.Fatalf("setup state=%+v err=%v", persisted, err)
	}
}
func TestCredentialRotationCASPreservesMetadataAndHistory(t *testing.T) {
	newServiceFixture(t, "callback")
	factory := &lifecycleFactory{}
	RegisterFactory(factory)
	yes := true
	p := &model.Payment{Type: lifecycleKind, Name: "测试", Currency: model.CurrencyTypeCNY, Config: `{"merchant":"merchant","key":"A"}`, Enable: &yes}
	if err := CreateGateway(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	metadata := *p
	metadata.Name = "新名称"
	rotated, err := RotateCredentials(context.Background(), p.ID, 1, `{"merchant":"merchant","key":"B"}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := metadata.Update(true); err != nil {
		t.Fatal(err)
	}
	stored, _ := model.GetPaymentByID(p.ID)
	if stored.CredentialRevision != 2 || stored.Config != rotated.Config || stored.Name != "新名称" {
		t.Fatalf("metadata overwrote credentials: %+v", stored)
	}
	if _, err := RotateCredentials(context.Background(), p.ID, 1, `{"merchant":"merchant","key":"C"}`); err == nil {
		t.Fatal("stale CAS accepted")
	}
	if _, err := RotateCredentials(context.Background(), p.ID, 2, `{"merchant":"other","key":"C"}`); err == nil {
		t.Fatal("identity changed")
	}
	if err := model.DB.Delete(p).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := RotateCredentials(context.Background(), p.ID, 2, `{"merchant":"merchant","key":"C"}`); err != nil {
		t.Fatalf("historical credential rotation blocked: %v", err)
	}
}
func TestCredentialCASFailureClosesCandidate(t *testing.T) {
	newServiceFixture(t, "callback")
	factory := &lifecycleFactory{}
	RegisterFactory(factory)
	p := &model.Payment{Type: lifecycleKind, Name: "测试", Currency: model.CurrencyTypeCNY, Config: `{"merchant":"merchant","key":"A"}`}
	if err := CreateGateway(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	factory.onCreate = func(snapshot types.GatewaySnapshot) {
		if snapshot.CredentialRevision == 2 {
			if err := model.DB.Model(&model.Payment{}).Where("id = ?", p.ID).Update("credential_revision", 3).Error; err != nil {
				t.Error(err)
			}
		}
	}
	if _, err := RotateCredentials(context.Background(), p.ID, 1, `{"merchant":"merchant","key":"B"}`); err == nil {
		t.Fatal("CAS race accepted")
	}
	candidate := factory.clients[len(factory.clients)-1]
	if candidate.stops.Load() != 1 {
		t.Fatal("CAS losing candidate leaked")
	}
}
