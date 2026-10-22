package jq

import (
	"context"
	"encoding/json"
	"testing"
)

func TestParseLegacyIdentity(t *testing.T) {
	raw := json.RawMessage(`{
		"loggedIn": true,
		"raw": {"data": {
			"isLogin": 1,
			"userId": "uid-abc",
			"uniqueKey": "uk",
			"alias": "nick",
			"vipType": "vip",
			"level": 2
		}}
	}`)
	call := func(method string, params any) (json.RawMessage, error) {
		if method != "auth.credits" {
			t.Fatalf("unexpected method %s", method)
		}
		return json.RawMessage(`{"amount":12,"known":true}`), nil
	}
	id, err := parseLegacyIdentity(raw, context.Background(), call)
	if err != nil {
		t.Fatal(err)
	}
	if !id.LoggedIn || id.UID != "uid-abc" || id.Nickname != "nick" || id.VIPType != "vip" {
		t.Fatalf("identity = %+v", id)
	}
	if !id.CreditsKnown || id.Credits != 12 {
		t.Fatalf("credits = %d known=%v", id.Credits, id.CreditsKnown)
	}
}

func TestParseLegacyIdentityLoggedOut(t *testing.T) {
	raw := json.RawMessage(`{"loggedIn": false, "raw": {"data": {"isLogin": 0}}}`)
	id, err := parseLegacyIdentity(raw, context.Background(), func(string, any) (json.RawMessage, error) {
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if id.LoggedIn || id.UID != "" {
		t.Fatalf("identity = %+v", id)
	}
}
