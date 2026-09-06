package mailbox

import (
        "encoding/json"
        "errors"
        "testing"
        "time"
)

func TestEnvelopeRoundTrip(t *testing.T) {
        key, err := DeriveKey("ab3d7k")
        if err != nil {
                t.Fatal(err)
        }
        plain := []byte(`{"proto":"clink","type":"offer","room":"ab3d7k"}`)
        sealed, err := Seal(key, plain)
        if err != nil {
                t.Fatal(err)
        }
        got, err := Open(key, sealed)
        if err != nil {
                t.Fatal(err)
        }
        if string(got) != string(plain) {
                t.Fatalf("roundtrip mismatch: %s != %s", got, plain)
        }
}

func TestEnvelopeWrongKey(t *testing.T) {
        key1, _ := DeriveKey("ab3d7k")
        key2, _ := DeriveKey("zzz999")
        sealed, err := Seal(key1, []byte("secret"))
        if err != nil {
                t.Fatal(err)
        }
        if _, err := Open(key2, sealed); err == nil {
                t.Fatal("用错误房间码应当解密失败")
        }
}

func TestEnvelopeTamperDetection(t *testing.T) {
        key, _ := DeriveKey("ab3d7k")
        sealed, _ := Seal(key, []byte("secret"))
        // 篡改密文末位
        tampered := sealed[:len(sealed)-2] + "AA"
        if _, err := Open(key, tampered); err == nil {
                t.Fatal("被篡改的信封应当解密失败（GCM 完整性校验）")
        }
}

func TestSlotCode(t *testing.T) {
        got := SlotCode("ab3d7k", 0, RoleOffer)
        if len(got) != 8 || got[:6] != "ab3d7k" || got[7] != 'h' {
                t.Fatalf("槽位码格式错误: %q", got)
        }
        ans := SlotCode("ab3d7k", 0, RoleAnswer)
        if ans[7] != 'g' {
                t.Fatalf("answer 槽位后缀错误: %q", ans)
        }
        e5 := SlotCode("ab3d7k", 5, RoleOffer)
        if e5[6] != Alphabet[5] {
                t.Fatalf("epoch 字符错误: %q", e5)
        }
}

func TestValidateRoomCode(t *testing.T) {
        if !ValidateRoomCode("ab3d7k") {
                t.Fatal("合法房间码被拒绝")
        }
        for _, bad := range []string{"ab3d7", "a13d7k", "A1B2C3", "ab3d7kk", "", "ab0d7k", "abid7k", "abld7k", "abod7k"} {
                if ValidateRoomCode(bad) {
                        t.Fatalf("非法房间码 %q 被接受", bad)
                }
        }
}

func TestGenRoomCode(t *testing.T) {
        for i := 0; i < 1000; i++ {
                code := GenRoomCode()
                if !ValidateRoomCode(code) {
                        t.Fatalf("生成的房间码非法: %q", code)
                }
        }
}

func TestSignalValidate(t *testing.T) {
        s := &Signal{V: ProtoVersion, Type: "offer", Ts: time.Now().Unix()}
        if err := s.Validate("offer"); err != nil {
                t.Fatal(err)
        }
        if err := s.Validate("answer"); err == nil {
                t.Fatal("类型不符应当报错")
        }
        old := &Signal{V: ProtoVersion, Type: "offer", Ts: time.Now().Add(-25 * time.Hour).Unix()}
        if err := old.Validate("offer"); err == nil {
                t.Fatal("过期房间应当报错")
        }
}

// TestLiveEndToEnd 对真实 Clipzy 服务做全链路验证（需网络）：
// 加密 → store → shorten(注册槽位) → redirect-info(寻址) → get → 解密。
func TestLiveEndToEnd(t *testing.T) {
        if testing.Short() {
                t.Skip("live network test")
        }
        c := NewClient("")
        room := GenRoomCode()
        key, err := DeriveKey(room)
        if err != nil {
                t.Fatal(err)
        }
        sig := Signal{
                V: ProtoVersion, Type: "offer", Epoch: 0, Tier: "dual",
                Nonce: Nonce(), Ts: time.Now().Unix(),
                SDP: "v=0\r\n-fake-sdp-for-test\r\n", SDPType: "offer",
                V6: "2408:8207::1", V6Port: 40210, MCPort: 25565, Name: "tester",
        }
        b, _ := json.Marshal(sig)
        sealed, err := Seal(key, b)
        if err != nil {
                t.Fatal(err)
        }

        id, err := c.Store(sealed, 600)
        if err != nil {
                t.Fatalf("store: %v", err)
        }
        slot := SlotCode(room, 0, RoleOffer)
        if err := c.Shorten(GitHubPagesRedirect+"?id="+id, slot); err != nil {
                t.Fatalf("shorten: %v", err)
        }

        gotURL, err := c.RedirectInfo(slot)
        if err != nil {
                t.Fatalf("redirect-info: %v", err)
        }
        id2, err := IDFromRedirectURL(gotURL)
        if err != nil {
                t.Fatal(err)
        }
        if id2 != id {
                t.Fatalf("寻址 id 不一致: %s != %s", id2, id)
        }

        sealed2, err := c.Get(id)
        if err != nil {
                t.Fatalf("get: %v", err)
        }
        plain, err := Open(key, sealed2)
        if err != nil {
                t.Fatalf("解密: %v", err)
        }
        var sig2 Signal
        if err := json.Unmarshal(plain, &sig2); err != nil {
                t.Fatal(err)
        }
        if sig2.SDP != sig.SDP || sig2.MCPort != sig.MCPort || sig2.V6 != sig.V6 {
                t.Fatalf("信令内容不一致: %+v", sig2)
        }

        // 占用语义：answer 槽注册一次成功、二次冲突
        ansSlot := SlotCode(room, 0, RoleAnswer)
        if err := c.Shorten(GitHubPagesRedirect+"?id="+id, ansSlot); err != nil {
                t.Fatalf("answer 槽注册: %v", err)
        }
        if err := c.Shorten(GitHubPagesRedirect+"?id="+id, ansSlot); !errors.Is(err, ErrCodeTaken) {
                t.Fatalf("重复注册应返回 ErrCodeTaken，实际: %v", err)
        }
}
