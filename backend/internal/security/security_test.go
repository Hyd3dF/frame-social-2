package security

import "testing"

func TestPairKeyIsStableAndOpaque(t *testing.T) {
	forward := PairKey("account:first", "account:second")
	reverse := PairKey("account:second", "account:first")
	if forward != reverse {
		t.Fatalf("pair key must be order independent: %q != %q", forward, reverse)
	}
	if len(forward) != 64 {
		t.Fatalf("pair key must be a SHA-256 hex string, got %d characters", len(forward))
	}
}

func TestOTPHashVerification(t *testing.T) {
	hash := OTPHash("a sufficiently long test pepper", "+905551112233", "123456")
	if !VerifyOTP("a sufficiently long test pepper", "+905551112233", "123456", hash) {
		t.Fatal("expected OTP to verify")
	}
	if VerifyOTP("a sufficiently long test pepper", "+905551112233", "654321", hash) {
		t.Fatal("wrong OTP must not verify")
	}
}
