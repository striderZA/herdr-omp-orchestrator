package task

import "testing"

// TestInboxDigestChangesWithEveryFactTheUserInspects pins what a settlement's
// digest covers: a change to any shown fact, including the delivery state and the
// detail, yields a different digest, so a confirmation cannot outlive what it showed.
func TestInboxDigestChangesWithEveryFactTheUserInspects(t *testing.T) {
	base := Inbox{
		ID: "n1", TaskID: "t1", Kind: "result", Summary: "finished",
		Delivery: "worker result", DeliveryState: DeliveryUncertain, DeliveryDetail: "inspect the pane",
	}
	if base.Digest() != base.Digest() {
		t.Fatal("the same note must have a stable digest")
	}
	changes := map[string]func(*Inbox){
		"task":    func(i *Inbox) { i.TaskID = "t2" },
		"kind":    func(i *Inbox) { i.Kind = "question" },
		"summary": func(i *Inbox) { i.Summary = "finished!" },
		"label":   func(i *Inbox) { i.Delivery = "worker progress" },
		"state":   func(i *Inbox) { i.DeliveryState = DeliveryPending },
		"detail":  func(i *Inbox) { i.DeliveryDetail = "inspected" },
	}
	for name, change := range changes {
		changed := base
		change(&changed)
		if changed.Digest() == base.Digest() {
			t.Errorf("changing the %s did not change the digest", name)
		}
	}
}

// Fields are joined without ambiguity: these two notes concatenate to the same
// text, so their digests must still differ.
func TestInboxDigestHasNoFieldBoundaryCollisions(t *testing.T) {
	if (Inbox{ID: "ab", Summary: "c"}).Digest() == (Inbox{ID: "a", Summary: "bc"}).Digest() {
		t.Fatal("ids and summaries that concatenate to the same text must not collide")
	}
}
