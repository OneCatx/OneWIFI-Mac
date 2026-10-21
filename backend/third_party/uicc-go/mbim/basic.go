package mbim

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

func (r *Reader) SubscriberReadyStatus(ctx context.Context) (SubscriberReadyStatusResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return SubscriberReadyStatusResponse{}, errors.New("reading MBIM subscriber ready status: reader is closed")
	}

	request := SubscriberReadyStatusRequest{TransactionID: r.nextTransactionID()}
	if err := request.Request().Transmit(ctx, r.conn); err != nil {
		return SubscriberReadyStatusResponse{}, fmt.Errorf("reading MBIM subscriber ready status: %w", err)
	}
	resp := *request.Response
	resp.TelephoneNumbers = slices.Clone(resp.TelephoneNumbers)
	return resp, nil
}
