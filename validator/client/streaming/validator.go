// Package streaming represents a gRPC stream-based implementation
// of an eth2 validator client.
package streaming

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dgraph-io/ristretto"
	"github.com/gogo/protobuf/proto"
	ptypes "github.com/gogo/protobuf/types"
	lru "github.com/hashicorp/golang-lru"
	"github.com/pkg/errors"
	ethpb "github.com/prysmaticlabs/ethereumapis/eth/v1alpha1"
	"github.com/prysmaticlabs/prysm/beacon-chain/core/helpers"
	slashpb "github.com/prysmaticlabs/prysm/proto/slashing"
	"github.com/prysmaticlabs/prysm/shared/bytesutil"
	"github.com/prysmaticlabs/prysm/shared/featureconfig"
	"github.com/prysmaticlabs/prysm/shared/hashutil"
	"github.com/prysmaticlabs/prysm/shared/params"
	"github.com/prysmaticlabs/prysm/shared/roughtime"
	"github.com/prysmaticlabs/prysm/shared/slotutil"
	"github.com/prysmaticlabs/prysm/validator/client/metrics"
	vdb "github.com/prysmaticlabs/prysm/validator/db"
	keymanager "github.com/prysmaticlabs/prysm/validator/keymanager/v1"
	slashingprotection "github.com/prysmaticlabs/prysm/validator/slashing-protection"
	"github.com/sirupsen/logrus"
	"go.opencensus.io/trace"
)

type validatorRole int8

const (
	roleUnknown = iota
	roleAttester
	roleProposer
	roleAggregator
)

type validator struct {
	genesisTime                        uint64
	ticker                             *slotutil.SlotTicker
	db                                 vdb.Database
	dutiesLock                         sync.RWMutex
	dutiesByEpoch                      map[uint64][]*ethpb.DutiesResponse_Duty
	validatorClient                    ethpb.BeaconNodeValidatorClient
	beaconClient                       ethpb.BeaconChainClient
	graffiti                           []byte
	node                               ethpb.NodeClient
	keyManager                         keymanager.KeyManager
	prevBalance                        map[[48]byte]uint64
	logValidatorBalances               bool
	emitAccountMetrics                 bool
	attLogs                            map[[32]byte]*attSubmitted
	attLogsLock                        sync.Mutex
	domainDataLock                     sync.Mutex
	domainDataCache                    *ristretto.Cache
	aggregatedSlotCommitteeIDCache     *lru.Cache
	aggregatedSlotCommitteeIDCacheLock sync.Mutex
	attesterHistoryByPubKey            map[[48]byte]*slashpb.AttestationHistory
	attesterHistoryByPubKeyLock        sync.RWMutex
	protector                          slashingprotection.Protector
}

// Done cleans up the validator.
func (v *validator) Done() {
	v.ticker.Done()
}

// WaitForChainStart checks whether the beacon node has started its runtime. That is,
// it calls to the beacon node which then verifies the ETH1.0 deposit contract logs to check
// for the ChainStart log to have been emitted. If so, it starts a ticker based on the ChainStart
// unix timestamp which will be used to keep track of time within the validator client.
func (v *validator) WaitForChainStart(ctx context.Context) error {
	ctx, span := trace.StartSpan(ctx, "validator.WaitForChainStart")
	defer span.End()
	// First, check if the beacon chain has started.
	stream, err := v.validatorClient.WaitForChainStart(ctx, &ptypes.Empty{})
	if err != nil {
		return errors.Wrap(err, "could not setup beacon chain ChainStart streaming client")
	}
	for {
		log.Info("Waiting for beacon chain start log from the ETH 1.0 deposit contract")
		chainStartRes, err := stream.Recv()
		// If the stream is closed, we stop the loop.
		if err == io.EOF {
			break
		}
		// If context is canceled we stop the loop.
		if ctx.Err() == context.Canceled {
			return errors.Wrap(ctx.Err(), "context has been canceled so shutting down the loop")
		}
		if err != nil {
			return errors.Wrap(err, "could not receive ChainStart from stream")
		}
		v.genesisTime = chainStartRes.GenesisTime
		break
	}
	// Once the ChainStart log is received, we update the genesis time of the validator client
	// and begin a slot ticker used to track the current slot the beacon node is in.
	v.ticker = slotutil.GetSlotTicker(time.Unix(int64(v.genesisTime), 0), params.BeaconConfig().SecondsPerSlot)
	log.WithField("genesisTime", time.Unix(int64(v.genesisTime), 0)).Info("Beacon chain genesis")
	return nil
}

// WaitForSync checks whether the beacon node has sync to the latest head.
func (v *validator) WaitForSync(ctx context.Context) error {
	ctx, span := trace.StartSpan(ctx, "validator.WaitForSync")
	defer span.End()

	s, err := v.node.GetSyncStatus(ctx, &ptypes.Empty{})
	if err != nil {
		return errors.Wrap(err, "could not get sync status")
	}
	if !s.Syncing {
		return nil
	}

	for {
		select {
		// Poll every half slot.
		case <-time.After(slotutil.DivideSlotBy(2 /* twice per slot */)):
			s, err := v.node.GetSyncStatus(ctx, &ptypes.Empty{})
			if err != nil {
				return errors.Wrap(err, "could not get sync status")
			}
			if !s.Syncing {
				return nil
			}
			log.Info("Waiting for beacon node to sync to latest chain head")
		case <-ctx.Done():
			return errors.New("context has been canceled, exiting goroutine")
		}
	}
}

// WaitForSynced opens a stream with the beacon chain node so it can be informed of when the beacon node is
// fully synced and ready to communicate with the validator.
func (v *validator) WaitForSynced(ctx context.Context) error {
	ctx, span := trace.StartSpan(ctx, "validator.WaitForSynced")
	defer span.End()
	// First, check if the beacon chain has started.
	stream, err := v.validatorClient.WaitForSynced(ctx, &ptypes.Empty{})
	if err != nil {
		return errors.Wrap(err, "could not setup beacon chain Synced streaming client")
	}
	for {
		log.Info("Waiting for chainstart to occur and the beacon node to be fully synced")
		syncedRes, err := stream.Recv()
		// If the stream is closed, we stop the loop.
		if err == io.EOF {
			break
		}
		// If context is canceled we stop the loop.
		if ctx.Err() == context.Canceled {
			return errors.Wrap(ctx.Err(), "context has been canceled so shutting down the loop")
		}
		if err != nil {
			return errors.Wrap(err, "could not receive Synced from stream")
		}
		v.genesisTime = syncedRes.GenesisTime
		break
	}
	// Once the Synced log is received, we update the genesis time of the validator client
	// and begin a slot ticker used to track the current slot the beacon node is in.
	v.ticker = slotutil.GetSlotTicker(time.Unix(int64(v.genesisTime), 0), params.BeaconConfig().SecondsPerSlot)
	log.WithField("genesisTime", time.Unix(int64(v.genesisTime), 0)).Info("Chain has started and the beacon node is synced")
	return nil
}

// WaitForActivation checks whether the validator pubkey is in the active
// validator set. If not, this operation will block until an activation message is
// received.
func (v *validator) WaitForActivation(ctx context.Context) error {
	ctx, span := trace.StartSpan(ctx, "validator.WaitForActivation")
	defer span.End()
	validatingKeys, err := v.keyManager.FetchValidatingKeys()
	if err != nil {
		return errors.Wrap(err, "could not fetch validating keys")
	}
	req := &ethpb.ValidatorActivationRequest{
		PublicKeys: bytesutil.FromBytes48Array(validatingKeys),
	}
	stream, err := v.validatorClient.WaitForActivation(ctx, req)
	if err != nil {
		return errors.Wrap(err, "could not setup validator WaitForActivation streaming client")
	}
	for {
		res, err := stream.Recv()
		// If the stream is closed, we stop the loop.
		if err == io.EOF {
			break
		}
		// If context is canceled we stop the loop.
		if ctx.Err() == context.Canceled {
			return errors.Wrap(ctx.Err(), "context has been canceled so shutting down the loop")
		}
		if err != nil {
			return errors.Wrap(err, "could not receive validator activation from stream")
		}
		valActivated := v.checkAndLogValidatorStatus(res.Statuses)

		if valActivated {
			for _, statusResp := range res.Statuses {
				if statusResp.Status.Status != ethpb.ValidatorStatus_ACTIVE {
					continue
				}
				log.WithFields(logrus.Fields{
					"publicKey": fmt.Sprintf("%#x", bytesutil.Trunc(statusResp.PublicKey)),
					"index":     statusResp.Index,
				}).Info("Validator activated")
			}
			break
		}
	}
	v.ticker = slotutil.GetSlotTicker(time.Unix(int64(v.genesisTime), 0), params.BeaconConfig().SecondsPerSlot)

	return nil
}

func (v *validator) checkAndLogValidatorStatus(validatorStatuses []*ethpb.ValidatorActivationResponse_Status) bool {
	nonexistentIndex := ^uint64(0)
	var validatorActivated bool
	for _, status := range validatorStatuses {
		fields := logrus.Fields{
			"pubKey": fmt.Sprintf("%#x", bytesutil.Trunc(status.PublicKey[:])),
			"status": status.Status.Status.String(),
		}
		if status.Index != nonexistentIndex {
			fields["index"] = status.Index
		}
		log := log.WithFields(fields)
		if v.emitAccountMetrics {
			fmtKey := fmt.Sprintf("%#x", status.PublicKey)
			metrics.ValidatorStatusesGaugeVec.WithLabelValues(fmtKey).Set(float64(status.Status.Status))
		}
		switch status.Status.Status {
		case ethpb.ValidatorStatus_UNKNOWN_STATUS:
			log.Info("Waiting for deposit to be observed by beacon node")
		case ethpb.ValidatorStatus_DEPOSITED:
			if status.Status.DepositInclusionSlot != 0 {
				log.WithFields(logrus.Fields{
					"expectedInclusionSlot":  status.Status.DepositInclusionSlot,
					"eth1DepositBlockNumber": status.Status.Eth1DepositBlockNumber,
				}).Info("Deposit for validator received but not processed into the beacon state")
			} else {
				log.WithField(
					"positionInActivationQueue", status.Status.PositionInActivationQueue,
				).Info("Deposit processed, entering activation queue after finalization")
			}
		case ethpb.ValidatorStatus_PENDING:
			if status.Status.ActivationEpoch == params.BeaconConfig().FarFutureEpoch {
				log.WithFields(logrus.Fields{
					"positionInActivationQueue": status.Status.PositionInActivationQueue,
				}).Info("Waiting to be assigned activation epoch")
			} else {
				log.WithFields(logrus.Fields{
					"activationEpoch": status.Status.ActivationEpoch,
				}).Info("Waiting for activation")
			}
		case ethpb.ValidatorStatus_ACTIVE:
			validatorActivated = true
		case ethpb.ValidatorStatus_EXITED:
			log.Info("Validator exited")
		default:
			log.WithFields(logrus.Fields{
				"activationEpoch": status.Status.ActivationEpoch,
			}).Info("Validator status")
		}
	}
	return validatorActivated
}

// NextSlot emits the next slot number at the start time of that slot.
func (v *validator) NextSlot() <-chan uint64 {
	return v.ticker.C()
}

// SlotDeadline is the start time of the next slot.
func (v *validator) SlotDeadline(slot uint64) time.Time {
	secs := (slot + 1) * params.BeaconConfig().SecondsPerSlot
	return time.Unix(int64(v.genesisTime), 0 /*ns*/).Add(time.Duration(secs) * time.Second)
}

// UpdateProtections goes through the duties of the given slot and fetches the required validator history,
// assigning it in validator.
func (v *validator) UpdateProtections(ctx context.Context, slot uint64) error {
	epoch := slot / params.BeaconConfig().SlotsPerEpoch
	v.dutiesLock.RLock()
	duty, ok := v.dutiesByEpoch[epoch]
	if !ok {
		v.dutiesLock.RUnlock()
		log.Debugf("No assigned duties yet for epoch %d", epoch)
		return nil
	}
	v.dutiesLock.RUnlock()
	attestingPubKeys := make([][48]byte, 0, len(duty))
	for _, dt := range duty {
		if dt == nil {
			continue
		}
		if dt.AttesterSlot == slot {
			attestingPubKeys = append(attestingPubKeys, bytesutil.ToBytes48(dt.PublicKey))
		}
	}
	attHistoryByPubKey, err := v.db.AttestationHistoryForPubKeys(ctx, attestingPubKeys)
	if err != nil {
		return errors.Wrap(err, "could not get attester history")
	}
	v.attesterHistoryByPubKey = attHistoryByPubKey
	return nil
}

// SaveProtections saves the attestation information currently in validator state.
func (v *validator) SaveProtections(ctx context.Context) error {
	if err := v.db.SaveAttestationHistoryForPubKeys(ctx, v.attesterHistoryByPubKey); err != nil {
		return errors.Wrap(err, "could not save attester history to DB")
	}
	v.attesterHistoryByPubKey = make(map[[48]byte]*slashpb.AttestationHistory)
	return nil
}

// isAggregator checks if a validator is an aggregator of a given slot, it uses the selection algorithm outlined in:
// https://github.com/ethereum/eth2.0-specs/blob/v0.9.3/specs/validator/0_beacon-chain-validator.md#aggregation-selection
func (v *validator) isAggregator(ctx context.Context, committee []uint64, slot uint64, pubKey [48]byte) (bool, error) {
	modulo := uint64(1)
	if len(committee)/int(params.BeaconConfig().TargetAggregatorsPerCommittee) > 1 {
		modulo = uint64(len(committee)) / params.BeaconConfig().TargetAggregatorsPerCommittee
	}

	slotSig, err := v.signSlot(ctx, pubKey, slot)
	if err != nil {
		return false, err
	}

	b := hashutil.Hash(slotSig)

	return binary.LittleEndian.Uint64(b[:8])%modulo == 0, nil
}

// UpdateDomainDataCaches by making calls for all of the possible domain data. These can change when
// the fork version changes which can happen once per epoch. Although changing for the fork version
// is very rare, a validator should check these data every epoch to be sure the validator is
// participating on the correct fork version.
func (v *validator) UpdateDomainDataCaches(ctx context.Context, slot uint64) {
	if !featureconfig.Get().EnableDomainDataCache {
		return
	}

	for _, d := range [][]byte{
		params.BeaconConfig().DomainRandao[:],
		params.BeaconConfig().DomainBeaconAttester[:],
		params.BeaconConfig().DomainBeaconProposer[:],
		params.BeaconConfig().DomainSelectionProof[:],
		params.BeaconConfig().DomainAggregateAndProof[:],
	} {
		_, err := v.domainData(ctx, helpers.SlotToEpoch(slot), d)
		if err != nil {
			log.WithError(err).Errorf("Failed to update domain data for domain %v", d)
		}
	}
}

// CurrentSlot based on the chain genesis time.
func (v *validator) CurrentSlot() uint64 {
	var currentSlot uint64
	genesisTime := time.Unix(int64(v.genesisTime), 0)
	if genesisTime.Before(roughtime.Now()) {
		currentSlot = slotutil.SlotsSinceGenesis(genesisTime)
	}
	return currentSlot
}

func (v *validator) domainData(ctx context.Context, epoch uint64, domain []byte) (*ethpb.DomainResponse, error) {
	v.domainDataLock.Lock()
	defer v.domainDataLock.Unlock()

	req := &ethpb.DomainRequest{
		Epoch:  epoch,
		Domain: domain,
	}

	key := strings.Join([]string{strconv.FormatUint(req.Epoch, 10), hex.EncodeToString(req.Domain)}, ",")

	if featureconfig.Get().EnableDomainDataCache {
		if val, ok := v.domainDataCache.Get(key); ok {
			return proto.Clone(val.(proto.Message)).(*ethpb.DomainResponse), nil
		}
	}

	res, err := v.validatorClient.DomainData(ctx, req)
	if err != nil {
		return nil, err
	}

	if featureconfig.Get().EnableDomainDataCache {
		v.domainDataCache.Set(key, proto.Clone(res), 1)
	}

	return res, nil
}

func (v *validator) logDuties(slot uint64, duties []*ethpb.DutiesResponse_Duty) {
	attesterKeys := make([][]string, params.BeaconConfig().SlotsPerEpoch)
	for i := range attesterKeys {
		attesterKeys[i] = make([]string, 0)
	}
	proposerKeys := make([]string, params.BeaconConfig().SlotsPerEpoch)
	slotOffset := helpers.StartSlot(helpers.SlotToEpoch(slot))

	for _, duty := range duties {
		if v.emitAccountMetrics {
			fmtKey := fmt.Sprintf("%#x", duty.PublicKey)
			metrics.ValidatorStatusesGaugeVec.WithLabelValues(fmtKey).Set(float64(duty.Status))
		}

		// Only interested in validators who are attesting/proposing.
		// Note that SLASHING validators will have duties but their results are ignored by the network so we don't bother with them.
		if duty.Status != ethpb.ValidatorStatus_ACTIVE && duty.Status != ethpb.ValidatorStatus_EXITING {
			continue
		}

		validatorKey := fmt.Sprintf("%#x", bytesutil.Trunc(duty.PublicKey))
		attesterIndex := duty.AttesterSlot - slotOffset
		if attesterIndex >= params.BeaconConfig().SlotsPerEpoch {
			log.WithField("duty", duty).Warn("Invalid attester slot")
		} else {
			attesterKeys[duty.AttesterSlot-slotOffset] = append(attesterKeys[duty.AttesterSlot-slotOffset], validatorKey)
		}

		for _, proposerSlot := range duty.ProposerSlots {
			proposerIndex := proposerSlot - slotOffset
			if proposerIndex >= params.BeaconConfig().SlotsPerEpoch {
				log.WithField("duty", duty).Warn("Invalid proposer slot")
			} else {
				proposerKeys[proposerIndex] = validatorKey
			}
		}
	}

	for i := uint64(0); i < params.BeaconConfig().SlotsPerEpoch; i++ {
		if len(attesterKeys[i]) > 0 {
			log.WithField("slot", slotOffset+i).WithField("attesters", len(attesterKeys[i])).WithField("pubKeys", attesterKeys[i]).Info("Attestation schedule")
		}
		if proposerKeys[i] != "" {
			log.WithField("slot", slotOffset+i).WithField("pubKey", proposerKeys[i]).Info("Proposal schedule")
		}
	}
}

// This constructs a validator subscribed key, it's used to track
// which subnet has already been pending requested.
func validatorSubscribeKey(slot uint64, committeeID uint64) [64]byte {
	return bytesutil.ToBytes64(append(bytesutil.Bytes32(slot), bytesutil.Bytes32(committeeID)...))
}
