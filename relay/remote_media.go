package relay

import (
	"context"
	"errors"

	"one-api/common/safefetch"
	"one-api/providers/base"
)

const remoteMediaFetchConcurrency = 8

var remoteMediaFetchAdmission = make(chan struct{}, remoteMediaFetchConcurrency)

type admittedRemoteMediaFetcher struct {
	ctx       context.Context
	admission chan struct{}
	fetch     func(context.Context, string) (string, []byte, error)
}

func newRequestRemoteMediaFetcher(ctx context.Context) base.RemoteMediaFetcher {
	return &admittedRemoteMediaFetcher{
		ctx:       ctx,
		admission: remoteMediaFetchAdmission,
		fetch:     safefetch.Fetch,
	}
}

func (f *admittedRemoteMediaFetcher) Fetch(rawURL string) (string, []byte, error) {
	if f == nil || f.ctx == nil {
		return "", nil, errors.New("remote media request context is unavailable")
	}
	if f.fetch == nil || f.admission == nil {
		return "", nil, errors.New("remote media fetcher is unavailable")
	}
	select {
	case f.admission <- struct{}{}:
		defer func() { <-f.admission }()
	case <-f.ctx.Done():
		return "", nil, f.ctx.Err()
	}
	return f.fetch(f.ctx, rawURL)
}

func prepareSelectedProviderRemoteMedia(relay RelayBaseInterface) error {
	preparer, ok := relay.(interface{ prepareSelectedProviderRemoteMedia() error })
	if !ok {
		return nil
	}
	return preparer.prepareSelectedProviderRemoteMedia()
}

func remoteMediaCapabilityGateError(err error) error {
	if err == nil {
		return nil
	}
	var capabilityErr *base.RequestCapabilityError
	if errors.As(err, &capabilityErr) {
		return newCapabilityGateError(capabilityErr.Param, capabilityErr.Message)
	}
	return newCapabilityGateError("messages", err.Error())
}
