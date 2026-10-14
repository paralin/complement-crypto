package tests

import (
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/matrix-org/complement-crypto/internal/api"
	"github.com/matrix-org/complement-crypto/internal/cc"
	"github.com/matrix-org/complement/ct"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

var boolTrue = true

type verificationStatus struct {
	SenderStage   api.VerificationStageTransitioned
	ReceiverStage api.VerificationStageTransitioned

	SenderDone   bool
	ReceiverDone bool

	mu *sync.Mutex
}

// done returns true iff the sender and receiver are both done.
func (s *verificationStatus) done(senderDone, receiverDone *bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if senderDone != nil {
		s.SenderDone = *senderDone
	}
	if receiverDone != nil {
		s.ReceiverDone = *receiverDone
	}
	return s.SenderDone && s.ReceiverDone
}

func (s *verificationStatus) attemptVerification(t *testing.T) {
	if s.ReceiverStage == nil || s.SenderStage == nil {
		return // we don't have emoji from both sides yet
	}
	senderEmoji := s.SenderStage.VerificationData().Emojis
	receiverEmoji := s.ReceiverStage.VerificationData().Emojis
	t.Logf("[SENDER]   %v", senderEmoji)
	t.Logf("[RECEIVER] %v", receiverEmoji)
	if reflect.DeepEqual(senderEmoji, receiverEmoji) {
		t.Logf("...it's a match!")
		s.SenderStage.Approve()
		s.ReceiverStage.Approve()
	} else {
		t.Logf("...mismatch detected.")
		s.SenderStage.Decline()
		s.ReceiverStage.Decline()
	}
}

// happy case test of Alice verifying one of her devices.
func TestVerificationSAS(t *testing.T) {
	Instance().ClientTypeMatrix(t, func(t *testing.T, verifierClientType, verifieeClientType api.ClientType) {
		if verifieeClientType.Lang == api.ClientTypeRust {
			t.Skipf("rust cannot be a verifiee yet, see https://github.com/matrix-org/matrix-rust-sdk/issues/3595")
		}
		tc := Instance().CreateTestContext(t, verifierClientType)
		verifieeUser := &cc.User{
			CSAPI:      tc.Alice.CSAPI,
			ClientType: verifieeClientType,
		}

		tc.WithAliceSyncing(t, func(verifier api.TestClient) {
			tc.WithClientSyncing(t, &cc.ClientCreationRequest{
				User: verifieeUser,
				Opts: api.ClientCreationOpts{
					DeviceID: "OTHER_DEVICE",
				},
			}, func(verifiee api.TestClient) {
				verifier.Logf(t, "Verifier (SENDER) %s %s", verifierClientType.Lang, verifier.Opts().DeviceID)
				verifiee.Logf(t, "Verifiee (RECEIVER) %s %s", verifieeClientType.Lang, verifiee.Opts().DeviceID)
				verifieeStage := verifiee.ListenForVerificationRequests(t)
				verifierStage := verifier.RequestOwnUserVerification(t)
				mustVerifySAS(t, verifierStage, verifieeStage)
			})
		})
	})
}

// happy case test of Alice verifying one of Bob's devices, which may be on another server.
func TestVerificationSASOtherUser(t *testing.T) {
	Instance().ClientTypeMatrix(t, func(t *testing.T, verifierClientType, verifieeClientType api.ClientType) {
		if verifierClientType.Lang == api.ClientTypeRust || verifieeClientType.Lang == api.ClientTypeRust {
			t.Skipf("rust cannot verify another user's device yet")
		}
		tc := Instance().CreateTestContext(t, verifierClientType, verifieeClientType)
		roomID := tc.CreateNewEncryptedRoom(
			t,
			tc.Alice,
			cc.EncRoomOptions.PresetTrustedPrivateChat(),
			cc.EncRoomOptions.Invite([]string{tc.Bob.UserID}),
		)
		tc.Bob.MustJoinRoom(t, roomID, []spec.ServerName{verifierClientType.HS})
		tc.WithAliceAndBobSyncing(t, func(verifier, verifiee api.TestClient) {
			// A client verifies only the devices of users it tracks, so make
			// Alice track Bob's devices by messaging him in the shared room.
			wantMsgBody := "Hello Bob"
			waiter := verifiee.WaitUntilEventInRoom(t, roomID, api.CheckEventHasBody(wantMsgBody))
			verifier.MustSendMessage(t, roomID, wantMsgBody)
			waiter.Waitf(t, 5*time.Second, "bob did not see alice's message")

			verifier.Logf(t, "Verifier (SENDER) %s %s", verifierClientType.Lang, verifier.Opts().DeviceID)
			verifiee.Logf(t, "Verifiee (RECEIVER) %s %s", verifieeClientType.Lang, verifiee.Opts().DeviceID)
			verifieeStage := verifiee.ListenForVerificationRequests(t)
			verifierStage := verifier.RequestDeviceVerification(t, verifiee.UserID(), verifiee.Opts().DeviceID)
			mustVerifySAS(t, verifierStage, verifieeStage)
		})
	})
}

// mustVerifySAS drives an SAS verification between a sender and a receiver
// until both are done, approving it if their emoji match.
func mustVerifySAS(t *testing.T, verifierStage, verifieeStage chan api.VerificationStage) {
	t.Helper()
	status := &verificationStatus{
		mu: &sync.Mutex{},
	}
	for {
		select {
		case receiverStage := <-verifieeStage:
			switch stage := receiverStage.(type) {
			case api.VerificationStageRequestedReceiver:
				t.Logf("[RECEIVER] VerificationStageRequestedReceiver: %+v", stage.Request())
				stage.Ready()
			case api.VerificationStageRequested:
				t.Logf("[RECEIVER] VerificationStageRequested: %+v", stage.Request())
			case api.VerificationStageReady:
				t.Logf("[RECEIVER] VerificationStageReady")
			case api.VerificationStageTransitioned:
				t.Logf("[RECEIVER] VerificationStageTransitioned")
				status.mu.Lock()
				status.ReceiverStage = stage
				status.attemptVerification(t)
				status.mu.Unlock()
			case api.VerificationStageStart:
				t.Logf("[RECEIVER] VerificationStageStart")
				stage.Transition()
			case api.VerificationStageDone:
				t.Logf("[RECEIVER] VerificationStageDone")
				if status.done(nil, &boolTrue) {
					return
				}
			case api.VerificationStageCancelled: // should not be cancelled
				ct.Errorf(t, "[RECEIVER] VerificationStageCancelled")
			}
		case senderStage := <-verifierStage:
			switch stage := senderStage.(type) {
			case api.VerificationStageRequestedReceiver: // the verifier should not get a requestee state
				ct.Errorf(t, "[SENDER]   VerificationStageRequestedReceiver: %+v", stage.Request())
			case api.VerificationStageRequested:
				t.Logf("[SENDER]   VerificationStageRequested: %+v", stage.Request())
			case api.VerificationStageReady:
				t.Logf("[SENDER]   VerificationStageReady: starting m.sas.v1")
				stage.Start("m.sas.v1")
			case api.VerificationStageTransitioned:
				t.Logf("[SENDER]   VerificationStageTransitioned")
				status.mu.Lock()
				status.SenderStage = stage
				status.attemptVerification(t)
				status.mu.Unlock()
			case api.VerificationStageStart:
				t.Logf("[SENDER]   VerificationStageStart")
				stage.Transition()
			case api.VerificationStageDone:
				t.Logf("[SENDER]   VerificationStageDone")
				if status.done(&boolTrue, nil) {
					return
				}
			case api.VerificationStageCancelled: // should not be cancelled
				ct.Errorf(t, "[SENDER]   VerificationStageCancelled")
			}
		case <-time.After(5 * time.Second):
			ct.Fatalf(t, "timed out after 5s")
			return
		}
	}
}
