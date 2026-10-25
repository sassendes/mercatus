package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	pipelineservice "mercatus/pipeline/service"
)

type fraudClient struct {
	url    string
	token  string
	client *http.Client
}

func newFraudClient(url, token string) *fraudClient {
	return &fraudClient{url: url, token: token, client: &http.Client{Timeout: 2 * time.Second}}
}

func (f *fraudClient) Score(ctx context.Context, buyerID, amount int64, newAccount bool, ipCountry string) (pipelineservice.FraudDecision, error) {
	body, _ := json.Marshal(map[string]any{"buyer_id": buyerID, "amount_cents": amount, "new_account": newAccount, "ip_country": ipCountry})
	d, err := f.score(ctx, body)
	if err == nil || ctx.Err() != nil {
		return d, err
	}
	select {
	case <-ctx.Done():
		return "", errors.Join(err, ctx.Err())
	case <-time.After(250 * time.Millisecond):
	}
	return f.score(ctx, body)
}

func (f *fraudClient) score(ctx context.Context, body []byte) (pipelineservice.FraudDecision, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.url+"/score", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if f.token != "" {
		req.Header.Set("Authorization", "Bearer "+f.token)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fraud engine status %d", resp.StatusCode)
	}
	var out struct {
		Decision string `json:"decision"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	switch d := pipelineservice.FraudDecision(out.Decision); d {
	case pipelineservice.FraudApprove, pipelineservice.FraudReview, pipelineservice.FraudDecline:
		return d, nil
	default:
		return "", fmt.Errorf("unknown fraud decision %q", out.Decision)
	}
}

func pingHTTP(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}
