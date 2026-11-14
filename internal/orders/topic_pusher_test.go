package orders

import (
	"context"
	"testing"
)

type recordingPusher struct{ topics []string }

func (r *recordingPusher) Push(_ context.Context, _, _, topic string, _ map[string]any) error {
	r.topics = append(r.topics, topic)
	return nil
}

// Each topic goes through its own pusher, and a topic without one goes to the fallback (issue 257).
func TestTopicPusherSendsEachTopicThroughItsOwnProject(t *testing.T) {
	rider, customer, stand := &recordingPusher{}, &recordingPusher{}, &recordingPusher{}
	p := TopicPusher{ByTopic: map[string]Pusher{"push.rider": rider, "push.customer": customer}, Fallback: stand}
	for _, topic := range []string{"push.rider", "push.customer", "push.customer", "push.other"} {
		if err := p.Push(context.Background(), "android", "tok", topic, nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(rider.topics) != 1 || len(customer.topics) != 2 || len(stand.topics) != 1 || stand.topics[0] != "push.other" {
		t.Errorf("rider %v, customer %v, fallback %v", rider.topics, customer.topics, stand.topics)
	}
	// A nil entry is the same as none.
	p.ByTopic["push.rider"] = nil
	_ = p.Push(context.Background(), "android", "tok", "push.rider", nil)
	if len(stand.topics) != 2 {
		t.Errorf("a nil pusher was used: %v", stand.topics)
	}
}
