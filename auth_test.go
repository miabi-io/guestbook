package main

import (
	"testing"
	"time"
)

func TestAdminSession(t *testing.T) {
	a := NewAdminAuth("s3cret")
	now := time.Now()
	v := a.Issue(now)

	if !a.Verify(v, now) {
		t.Fatal("fresh session should verify")
	}
	if a.Verify(v, now.Add(sessionTTL+time.Second)) {
		t.Fatal("expired session should not verify")
	}
	if NewAdminAuth("other").Verify(v, now) {
		t.Fatal("session signed with another token should not verify")
	}
	if a.Verify("9999999999."+v[len(v)-64:], now) {
		t.Fatal("tampered expiry should not verify")
	}
}

func TestAdminDisabled(t *testing.T) {
	a := NewAdminAuth("")
	if a.Enabled() || a.CheckToken("") || a.Verify(a.Issue(time.Now()), time.Now()) {
		t.Fatal("empty token must disable all admin access")
	}
}
