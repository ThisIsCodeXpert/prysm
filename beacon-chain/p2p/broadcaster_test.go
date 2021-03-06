package p2p

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/gogo/protobuf/proto"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	eth "github.com/prysmaticlabs/ethereumapis/eth/v1alpha1"
	"github.com/prysmaticlabs/prysm/beacon-chain/core/helpers"
	p2ptest "github.com/prysmaticlabs/prysm/beacon-chain/p2p/testing"
	testpb "github.com/prysmaticlabs/prysm/proto/testing"
	"github.com/prysmaticlabs/prysm/shared/testutil"
	"github.com/prysmaticlabs/prysm/shared/testutil/assert"
	"github.com/prysmaticlabs/prysm/shared/testutil/require"
)

func TestService_Broadcast(t *testing.T) {
	p1 := p2ptest.NewTestP2P(t)
	p2 := p2ptest.NewTestP2P(t)
	p1.Connect(p2)
	if len(p1.BHost.Network().Peers()) == 0 {
		t.Fatal("No peers")
	}

	p := &Service{
		host:                  p1.BHost,
		pubsub:                p1.PubSub(),
		joinedTopics:          map[string]*pubsub.Topic{},
		cfg:                   &Config{},
		genesisTime:           time.Now(),
		genesisValidatorsRoot: []byte{'A'},
	}

	msg := &testpb.TestSimpleMessage{
		Bar: 55,
	}

	topic := "/eth2/%x/testing"
	// Set a test gossip mapping for testpb.TestSimpleMessage.
	GossipTypeMapping[reflect.TypeOf(msg)] = topic
	digest, err := p.forkDigest()
	require.NoError(t, err)
	topic = fmt.Sprintf(topic, digest)

	// External peer subscribes to the topic.
	topic += p.Encoding().ProtocolSuffix()
	sub, err := p2.SubscribeToTopic(topic)
	require.NoError(t, err)

	time.Sleep(50 * time.Millisecond) // libp2p fails without this delay...

	// Async listen for the pubsub, must be before the broadcast.
	var wg sync.WaitGroup
	wg.Add(1)
	go func(tt *testing.T) {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()

		incomingMessage, err := sub.Next(ctx)
		require.NoError(t, err)

		result := &testpb.TestSimpleMessage{}
		require.NoError(t, p.Encoding().DecodeGossip(incomingMessage.Data, result))
		if !proto.Equal(result, msg) {
			tt.Errorf("Did not receive expected message, got %+v, wanted %+v", result, msg)
		}
	}(t)

	// Broadcast to peers and wait.
	require.NoError(t, p.Broadcast(context.Background(), msg))
	if testutil.WaitTimeout(&wg, 1*time.Second) {
		t.Error("Failed to receive pubsub within 1s")
	}
}

func TestService_Broadcast_ReturnsErr_TopicNotMapped(t *testing.T) {
	p := Service{
		genesisTime:           time.Now(),
		genesisValidatorsRoot: []byte{'A'},
	}
	assert.ErrorContains(t, ErrMessageNotMapped.Error(), p.Broadcast(context.Background(), &testpb.AddressBook{}))
}

func TestService_Attestation_Subnet(t *testing.T) {
	if gtm := GossipTypeMapping[reflect.TypeOf(&eth.Attestation{})]; gtm != AttestationSubnetTopicFormat {
		t.Errorf("Constant is out of date. Wanted %s, got %s", AttestationSubnetTopicFormat, gtm)
	}

	tests := []struct {
		att   *eth.Attestation
		topic string
	}{
		{
			att: &eth.Attestation{
				Data: &eth.AttestationData{
					CommitteeIndex: 0,
					Slot:           2,
				},
			},
			topic: "/eth2/00000000/beacon_attestation_2",
		},
		{
			att: &eth.Attestation{
				Data: &eth.AttestationData{
					CommitteeIndex: 11,
					Slot:           10,
				},
			},
			topic: "/eth2/00000000/beacon_attestation_21",
		},
		{
			att: &eth.Attestation{
				Data: &eth.AttestationData{
					CommitteeIndex: 55,
					Slot:           529,
				},
			},
			topic: "/eth2/00000000/beacon_attestation_8",
		},
	}
	for _, tt := range tests {
		subnet := helpers.ComputeSubnetFromCommitteeAndSlot(100, tt.att.Data.CommitteeIndex, tt.att.Data.Slot)
		assert.Equal(t, tt.topic, attestationToTopic(subnet, [4]byte{} /* fork digest */), "Wrong topic")
	}
}
