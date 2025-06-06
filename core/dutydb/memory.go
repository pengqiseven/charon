// Copyright © 2022-2025 Obol Labs Inc. Licensed under the terms of a Business Source License 1.1

package dutydb

import (
	"context"
	"encoding/hex"
	"sync"

	eth2api "github.com/attestantio/go-eth2-client/api"
	eth2spec "github.com/attestantio/go-eth2-client/spec"
	"github.com/attestantio/go-eth2-client/spec/altair"
	eth2p0 "github.com/attestantio/go-eth2-client/spec/phase0"

	"github.com/obolnetwork/charon/app/errors"
	"github.com/obolnetwork/charon/app/z"
	"github.com/obolnetwork/charon/core"
)

const (
	_ = iota
	attV1
	attV2
)

// NewMemDB returns a new in-memory dutyDB instance.
func NewMemDB(deadliner core.Deadliner) *MemDB {
	return &MemDB{
		attDuties:         make(map[attKey]*eth2p0.AttestationData),
		attPubKeys:        make(map[pkKey]*core.PubKey),
		attV2PubKeys:      make(map[pkKeyV2]*core.PubKey),
		attKeysBySlot:     make(map[uint64][]pkKey),
		attV2KeysBySlot:   make(map[uint64][]pkKeyV2),
		proDuties:         make(map[uint64]*eth2api.VersionedProposal),
		aggDuties:         make(map[aggKey]core.AggregatedAttestation),
		aggDutiesV2:       make(map[aggKey]core.VersionedAggregatedAttestation),
		aggKeysBySlot:     make(map[uint64][]aggKey),
		contribDuties:     make(map[contribKey]*altair.SyncCommitteeContribution),
		contribKeysBySlot: make(map[uint64][]contribKey),
		shutdown:          make(chan struct{}),
		deadliner:         deadliner,
	}
}

// MemDB is an in-memory dutyDB implementation.
// It is a placeholder for the badgerDB implementation.
type MemDB struct {
	mu sync.Mutex

	// DutyAttester
	attDuties       map[attKey]*eth2p0.AttestationData
	attPubKeys      map[pkKey]*core.PubKey
	attV2PubKeys    map[pkKeyV2]*core.PubKey
	attKeysBySlot   map[uint64][]pkKey
	attV2KeysBySlot map[uint64][]pkKeyV2
	attQueries      []attQuery

	// DutyProposer
	proDuties  map[uint64]*eth2api.VersionedProposal
	proQueries []proQuery

	// DutyAggregator
	aggDuties     map[aggKey]core.AggregatedAttestation
	aggDutiesV2   map[aggKey]core.VersionedAggregatedAttestation
	aggKeysBySlot map[uint64][]aggKey
	aggQueries    []aggQuery
	aggQueriesV2  []aggQueryV2

	// DutySyncContribution
	contribDuties     map[contribKey]*altair.SyncCommitteeContribution
	contribKeysBySlot map[uint64][]contribKey
	contribQueries    []contribQuery

	shutdown  chan struct{}
	deadliner core.Deadliner
}

// Shutdown results in all blocking queries to return shutdown errors.
// Note this may only be called *once*.
func (db *MemDB) Shutdown() {
	close(db.shutdown)
}

// Store implements core.DutyDB, see its godoc.
func (db *MemDB) Store(_ context.Context, duty core.Duty, unsignedSet core.UnsignedDataSet) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	if !db.deadliner.Add(duty) {
		return errors.New("not storing unsigned data for expired duty", z.Any("duty", duty))
	}

	switch duty.Type {
	case core.DutyProposer:
		// Sanity check max one proposer per slot
		if len(unsignedSet) > 1 {
			return errors.New("unexpected proposer data set length", z.Int("n", len(unsignedSet)))
		}
		for _, unsignedData := range unsignedSet {
			err := db.storeProposalUnsafe(unsignedData)
			if err != nil {
				return err
			}
		}
		db.resolveProQueriesUnsafe()
	case core.DutyBuilderProposer:
		return core.ErrDeprecatedDutyBuilderProposer
	case core.DutyAttester:
		for pubkey, unsignedData := range unsignedSet {
			err := db.storeAttestationUnsafe(pubkey, unsignedData)
			if err != nil {
				return err
			}
		}
		db.resolveAttQueriesUnsafe()
	case core.DutyAggregator:
		version := 0
		var err error
		for _, unsignedData := range unsignedSet {
			version, err = db.storeAggAttestationUnsafe(unsignedData)
			if err != nil {
				return err
			}
		}
		switch version {
		case attV1:
			db.resolveAggQueriesUnsafe()
		case attV2:
			db.resolveAggQueriesV2Unsafe()
		}
	case core.DutySyncContribution:
		for _, unsignedData := range unsignedSet {
			err := db.storeSyncContributionUnsafe(unsignedData)
			if err != nil {
				return err
			}
		}
		db.resolveContribQueriesUnsafe()
	default:
		return errors.New("unsupported duty type", z.Str("type", duty.Type.String()))
	}

	// Delete all expired duties.
	for {
		var deleted bool
		select {
		case duty := <-db.deadliner.C():
			err := db.deleteDutyUnsafe(duty)
			if err != nil {
				return err
			}
			deleted = true
		default:
		}

		if !deleted {
			break
		}
	}

	return nil
}

// AwaitProposal implements core.DutyDB, see its godoc.
func (db *MemDB) AwaitProposal(ctx context.Context, slot uint64) (*eth2api.VersionedProposal, error) {
	cancel := make(chan struct{})
	defer close(cancel)
	response := make(chan *eth2api.VersionedProposal, 1)

	db.mu.Lock()
	db.proQueries = append(db.proQueries, proQuery{
		Key:      slot,
		Response: response,
		Cancel:   cancel,
	})
	db.resolveProQueriesUnsafe()
	db.mu.Unlock()

	select {
	case <-db.shutdown:
		return nil, errors.New("dutydb shutdown")
	case <-ctx.Done():
		return nil, ctx.Err()
	case block := <-response:
		return block, nil
	}
}

// AwaitAttestation implements core.DutyDB, see its godoc.
func (db *MemDB) AwaitAttestation(ctx context.Context, slot uint64, commIdx uint64) (*eth2p0.AttestationData, error) {
	cancel := make(chan struct{})
	defer close(cancel)
	response := make(chan *eth2p0.AttestationData, 1) // Instance of one so resolving never blocks

	db.mu.Lock()
	db.attQueries = append(db.attQueries, attQuery{
		Key: attKey{
			Slot:    slot,
			CommIdx: commIdx,
		},
		Response: response,
		Cancel:   cancel,
	})
	db.resolveAttQueriesUnsafe()
	db.mu.Unlock()

	select {
	case <-db.shutdown:
		return nil, errors.New("dutydb shutdown")
	case <-ctx.Done():
		return nil, ctx.Err()
	case value := <-response:
		return value, nil
	}
}

// AwaitAggAttestation blocks and returns the aggregated attestation for the slot
// and attestation when available.
func (db *MemDB) AwaitAggAttestation(ctx context.Context, slot uint64, attestationRoot eth2p0.Root,
) (*eth2p0.Attestation, error) {
	cancel := make(chan struct{})
	defer close(cancel)
	response := make(chan core.AggregatedAttestation, 1)                   // Instance of one so resolving never blocks
	responseVersioned := make(chan core.VersionedAggregatedAttestation, 1) // Instance of one so resolving never blocks

	db.mu.Lock()
	db.aggQueries = append(db.aggQueries, aggQuery{
		Key: aggKey{
			Slot: slot,
			Root: attestationRoot,
		},
		Response: response,
		Cancel:   cancel,
	})
	db.resolveAggQueriesUnsafe()
	db.aggQueriesV2 = append(db.aggQueriesV2, aggQueryV2{
		Key: aggKey{
			Slot: slot,
			Root: attestationRoot,
		},
		Response: responseVersioned,
		Cancel:   cancel,
	})
	db.resolveAggQueriesV2Unsafe()
	db.mu.Unlock()

	select {
	case <-db.shutdown:
		return nil, errors.New("dutydb shutdown")
	case <-ctx.Done():
		return nil, ctx.Err()
	case value := <-response:
		// Clone before returning.
		clone, err := value.Clone()
		if err != nil {
			return nil, err
		}
		aggAtt, ok := clone.(core.AggregatedAttestation)
		if !ok {
			return nil, errors.New("invalid aggregated attestation")
		}

		return &aggAtt.Attestation, nil
	case valueVersioned := <-responseVersioned:
		// Clone before returning.
		clone, err := valueVersioned.Clone()
		if err != nil {
			return nil, err
		}
		aggAtt, ok := clone.(core.VersionedAggregatedAttestation)
		if !ok {
			return nil, errors.New("invalid versioned aggregated attestation")
		}

		switch aggAtt.Version {
		case eth2spec.DataVersionPhase0:
			return aggAtt.Phase0, nil
		case eth2spec.DataVersionAltair:
			return aggAtt.Altair, nil
		case eth2spec.DataVersionBellatrix:
			return aggAtt.Bellatrix, nil
		case eth2spec.DataVersionCapella:
			return aggAtt.Capella, nil
		case eth2spec.DataVersionDeneb:
			return aggAtt.Deneb, nil
		default:
			return nil, errors.New("unsupported versioned aggregated attestation for v1 endpoint", z.Str("version", aggAtt.Version.String()))
		}
	}
}

// AwaitAggAttestationV2 blocks and returns the aggregated attestation for the slot
// and attestation when available.
func (db *MemDB) AwaitAggAttestationV2(ctx context.Context, slot uint64, attestationRoot eth2p0.Root,
) (*eth2spec.VersionedAttestation, error) {
	cancel := make(chan struct{})
	defer close(cancel)
	response := make(chan core.AggregatedAttestation, 1)                   // Instance of one so resolving never blocks
	versionedResponse := make(chan core.VersionedAggregatedAttestation, 1) // Instance of one so resolving never blocks

	db.mu.Lock()
	db.aggQueries = append(db.aggQueries, aggQuery{
		Key: aggKey{
			Slot: slot,
			Root: attestationRoot,
		},
		Response: response,
		Cancel:   cancel,
	})
	db.resolveAggQueriesUnsafe()
	db.aggQueriesV2 = append(db.aggQueriesV2, aggQueryV2{
		Key: aggKey{
			Slot: slot,
			Root: attestationRoot,
		},
		Response: versionedResponse,
		Cancel:   cancel,
	})
	db.resolveAggQueriesV2Unsafe()
	db.mu.Unlock()

	select {
	case <-db.shutdown:
		return nil, errors.New("dutydb shutdown")
	case <-ctx.Done():
		return nil, ctx.Err()
	case versionedValue := <-versionedResponse:
		// Clone before returning.
		clone, err := versionedValue.Clone()
		if err != nil {
			return nil, err
		}
		aggAtt, ok := clone.(core.VersionedAggregatedAttestation)
		if !ok {
			return nil, errors.New("invalid aggregated attestation")
		}

		return &aggAtt.VersionedAttestation, nil
	case value := <-response:
		// Clone before returning.
		clone, err := value.Clone()
		if err != nil {
			return nil, err
		}
		aggAtt, ok := clone.(core.AggregatedAttestation)
		if !ok {
			return nil, errors.New("invalid aggregated attestation")
		}

		return &eth2spec.VersionedAttestation{
			Version: eth2spec.DataVersionDeneb,
			Deneb:   &aggAtt.Attestation,
		}, nil
	}
}

// AwaitSyncContribution blocks and returns the sync committee contribution data for the slot and
// the subcommittee and the beacon block root when available.
func (db *MemDB) AwaitSyncContribution(ctx context.Context, slot, subcommIdx uint64, beaconBlockRoot eth2p0.Root) (*altair.SyncCommitteeContribution, error) {
	cancel := make(chan struct{})
	defer close(cancel)
	response := make(chan *altair.SyncCommitteeContribution, 1) // Instance of one so resolving never blocks

	db.mu.Lock()
	db.contribQueries = append(db.contribQueries, contribQuery{
		Key: contribKey{
			Slot:       slot,
			SubcommIdx: subcommIdx,
			Root:       beaconBlockRoot,
		},
		Response: response,
		Cancel:   cancel,
	})
	db.resolveContribQueriesUnsafe()
	db.mu.Unlock()

	select {
	case <-db.shutdown:
		return nil, errors.New("dutydb shutdown")
	case <-ctx.Done():
		return nil, ctx.Err()
	case value := <-response:
		return value, nil
	}
}

// PubKeyByAttestation implements core.DutyDB, see its godoc.
func (db *MemDB) PubKeyByAttestation(_ context.Context, slot, commIdx, valCommIdx uint64) (core.PubKey, error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	key := pkKey{
		Slot:       slot,
		CommIdx:    commIdx,
		ValCommIdx: valCommIdx,
	}

	pubkey, ok := db.attPubKeys[key]
	if !ok {
		return "", errors.New("pubkey not found")
	}

	return *pubkey, nil
}

// PubKeyByAttestationV2 implements core.DutyDB, see its godoc.
func (db *MemDB) PubKeyByAttestationV2(_ context.Context, slot, commIdx, valIdx uint64) (core.PubKey, error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	key := pkKeyV2{
		Slot:    slot,
		CommIdx: commIdx,
		ValIdx:  valIdx,
	}

	pubkey, ok := db.attV2PubKeys[key]
	if !ok {
		return "", errors.New("pubkey not found")
	}

	return *pubkey, nil
}

// storeAttestationUnsafe stores the unsigned attestation. It is unsafe since it assumes the lock is held.
func (db *MemDB) storeAttestationUnsafe(pubkey core.PubKey, unsignedData core.UnsignedData) error {
	cloned, err := unsignedData.Clone() // Clone before storing.
	if err != nil {
		return err
	}

	attData, ok := cloned.(core.AttestationData)
	if !ok {
		return errors.New("invalid unsigned attestation data")
	}

	// Store the same pubkey in v1 and v2 maps in order to avoid data duplication.
	pubkeyStore := &pubkey

	// Store key and value for PubKeyByAttestation
	pKey := pkKey{
		Slot:       uint64(attData.Data.Slot),
		CommIdx:    uint64(attData.Duty.CommitteeIndex),
		ValCommIdx: attData.Duty.ValidatorCommitteeIndex,
	}

	if value, ok := db.attPubKeys[pKey]; ok {
		if *value != *pubkeyStore {
			return errors.New("clashing public key", z.Any("pKey", pKey))
		}
	} else {
		db.attPubKeys[pKey] = pubkeyStore
		db.attKeysBySlot[uint64(attData.Duty.Slot)] = append(db.attKeysBySlot[uint64(attData.Duty.Slot)], pKey)
	}

	// Store key and value for PubKeyByAttestationV2
	pKeyV2 := pkKeyV2{
		Slot:    uint64(attData.Data.Slot),
		CommIdx: uint64(attData.Duty.CommitteeIndex),
		ValIdx:  uint64(attData.Duty.ValidatorIndex),
	}

	if value, ok := db.attV2PubKeys[pKeyV2]; ok {
		if *value != *pubkeyStore {
			return errors.New("clashing public key", z.Any("pKeyV2", pKeyV2))
		}
	} else {
		db.attV2PubKeys[pKeyV2] = pubkeyStore
		db.attV2KeysBySlot[uint64(attData.Duty.Slot)] = append(db.attV2KeysBySlot[uint64(attData.Duty.Slot)], pKeyV2)
	}

	// Store key and value for AwaitAttestation
	aKey := attKey{
		Slot:    uint64(attData.Data.Slot),
		CommIdx: uint64(attData.Duty.CommitteeIndex),
	}

	if value, ok := db.attDuties[aKey]; ok {
		if value.String() != attData.Data.String() {
			return errors.New("clashing attestation data", z.Any("key", aKey))
		}
	} else {
		db.attDuties[aKey] = &attData.Data
	}

	return nil
}

// storeAggAttestationUnsafe stores the unsigned aggregated attestation. It is unsafe since it assumes the lock is held.
func (db *MemDB) storeAggAttestationUnsafe(unsignedData core.UnsignedData) (int, error) {
	cloned, err := unsignedData.Clone() // Clone before storing.
	if err != nil {
		return 0, err
	}

	versionedAggAtt, ok := cloned.(core.VersionedAggregatedAttestation)
	if !ok {
		aggAtt, ok := cloned.(core.AggregatedAttestation)
		if !ok {
			return 0, errors.New("invalid unsigned aggregated attestation")
		}

		return attV1, db.storeAggAttestationUnsafeV1(aggAtt)
	}

	return attV2, db.storeAggAttestationUnsafeV2(versionedAggAtt)
}

// storeAggAttestationUnsafeV1 stores the unsigned aggregated attestation. It is unsafe since it assumes the lock is held.
func (db *MemDB) storeAggAttestationUnsafeV1(aggAtt core.AggregatedAttestation) error {
	aggRoot, err := aggAtt.Data.HashTreeRoot()
	if err != nil {
		return errors.Wrap(err, "hash aggregated attestation root")
	}

	slot := uint64(aggAtt.Data.Slot)

	// Store key and value for PubKeyByAttestation
	key := aggKey{
		Slot: slot,
		Root: aggRoot,
	}
	if existing, ok := db.aggDuties[key]; ok {
		existingRoot, err := existing.HashTreeRoot()
		if err != nil {
			return errors.Wrap(err, "attestation root")
		}

		providedRoot, err := aggAtt.HashTreeRoot()
		if err != nil {
			return errors.Wrap(err, "attestation root")
		}

		if existingRoot != providedRoot {
			return errors.New("clashing aggregated attestation")
		}
	} else {
		db.aggDuties[key] = aggAtt
		db.aggKeysBySlot[slot] = append(db.aggKeysBySlot[slot], key)
	}

	return nil
}

// storeAggAttestationUnsafeV2 stores the unsigned versioned aggregated attestation. It is unsafe since it assumes the lock is held.
func (db *MemDB) storeAggAttestationUnsafeV2(aggAtt core.VersionedAggregatedAttestation) error {
	aggAttData, err := aggAtt.Data()
	if err != nil {
		return err
	}
	aggRoot, err := aggAttData.HashTreeRoot()
	if err != nil {
		return errors.Wrap(err, "hash aggregated attestation root")
	}

	slot := uint64(aggAttData.Slot)

	// Store key and value for PubKeyByAttestation
	key := aggKey{
		Slot: slot,
		Root: aggRoot,
	}
	if existing, ok := db.aggDutiesV2[key]; ok {
		existingData, err := existing.Data()
		if err != nil {
			return errors.Wrap(err, "existing data")
		}
		existingDataRoot, err := existingData.HashTreeRoot()
		if err != nil {
			return errors.Wrap(err, "existing data root")
		}

		provided := aggAtt
		providedData, err := provided.Data()
		if err != nil {
			return errors.Wrap(err, "provided data")
		}
		providedDataRoot, err := providedData.HashTreeRoot()
		if err != nil {
			return errors.Wrap(err, "provided data root")
		}

		if existingDataRoot != providedDataRoot {
			return errors.New("clashing data root", z.Str("existing", hex.EncodeToString(existingDataRoot[:])), z.Str("provided", hex.EncodeToString(providedDataRoot[:])))
		}

		switch existing.Version {
		case eth2spec.DataVersionPhase0, eth2spec.DataVersionAltair, eth2spec.DataVersionBellatrix, eth2spec.DataVersionCapella, eth2spec.DataVersionDeneb:
			return errors.New("incorrect version for storing aggregation attestation v2")
		case eth2spec.DataVersionElectra:
			db.aggDutiesV2[key] = provided
		default:
			return errors.New("unknown version")
		}
	} else {
		db.aggDutiesV2[key] = aggAtt
		db.aggKeysBySlot[slot] = append(db.aggKeysBySlot[slot], key)
	}

	return nil
}

// storeSyncContributionUnsafe stores the unsigned aggregated attestation. It is unsafe since it assumes the lock is held.
func (db *MemDB) storeSyncContributionUnsafe(unsignedData core.UnsignedData) error {
	cloned, err := unsignedData.Clone() // Clone before storing.
	if err != nil {
		return err
	}

	contrib, ok := cloned.(core.SyncContribution)
	if !ok {
		return errors.New("invalid unsigned sync committee contribution")
	}

	contribRoot, err := contrib.HashTreeRoot()
	if err != nil {
		return errors.Wrap(err, "hash sync committee contribution")
	}

	key := contribKey{
		Slot:       uint64(contrib.Slot),
		SubcommIdx: contrib.SubcommitteeIndex,
		Root:       contrib.BeaconBlockRoot,
	}

	if existing, ok := db.contribDuties[key]; ok {
		existingRoot, err := existing.HashTreeRoot()
		if err != nil {
			return errors.Wrap(err, "sync committee contribution root")
		}

		if existingRoot != contribRoot {
			return errors.New("clashing sync contributions")
		}
	} else {
		db.contribDuties[key] = &contrib.SyncCommitteeContribution
		db.contribKeysBySlot[uint64(contrib.Slot)] = append(db.contribKeysBySlot[uint64(contrib.Slot)], key)
	}

	return nil
}

// storeProposalUnsafe stores the unsigned Proposal. It is unsafe since it assumes the lock is held.
func (db *MemDB) storeProposalUnsafe(unsignedData core.UnsignedData) error {
	cloned, err := unsignedData.Clone() // Clone before storing.
	if err != nil {
		return err
	}

	proposal, ok := cloned.(core.VersionedProposal)
	if !ok {
		return errors.New("invalid versioned proposal")
	}

	slot, err := proposal.Slot()
	if err != nil {
		return err
	}

	if existing, ok := db.proDuties[uint64(slot)]; ok {
		existingRoot, err := existing.Root()
		if err != nil {
			return errors.Wrap(err, "proposal root")
		}

		providedRoot, err := proposal.Root()
		if err != nil {
			return errors.Wrap(err, "proposal root")
		}

		if existingRoot != providedRoot {
			return errors.New("clashing blocks")
		}
	} else {
		db.proDuties[uint64(slot)] = &proposal.VersionedProposal
	}

	return nil
}

// resolveAttQueriesUnsafe resolve any attQuery to a result if found.
// It is unsafe since it assume that the lock is held.
func (db *MemDB) resolveAttQueriesUnsafe() {
	var unresolved []attQuery
	for _, query := range db.attQueries {
		if cancelled(query.Cancel) {
			continue // Drop cancelled queries.
		}

		value, ok := db.attDuties[query.Key]
		if !ok {
			unresolved = append(unresolved, query)
			continue
		}

		query.Response <- value
	}

	db.attQueries = unresolved
}

// resolveProQueriesUnsafe resolve any proQuery to a result if found.
// It is unsafe since it assume that the lock is held.
func (db *MemDB) resolveProQueriesUnsafe() {
	var unresolved []proQuery
	for _, query := range db.proQueries {
		if cancelled(query.Cancel) {
			continue // Drop cancelled queries.
		}

		value, ok := db.proDuties[query.Key]
		if !ok {
			unresolved = append(unresolved, query)
			continue
		}

		query.Response <- value
	}

	db.proQueries = unresolved
}

// resolveAggQueriesUnsafe resolve any aggQuery to a result if found.
// It is unsafe since it assume that the lock is held.
func (db *MemDB) resolveAggQueriesUnsafe() {
	var unresolved []aggQuery
	for _, query := range db.aggQueries {
		if cancelled(query.Cancel) {
			continue // Drop cancelled queries.
		}

		value, ok := db.aggDuties[query.Key]
		if !ok {
			unresolved = append(unresolved, query)
			continue
		}

		query.Response <- value
	}

	db.aggQueries = unresolved
}

// resolveAggQueriesV2Unsafe resolve any aggQueryV2 to a result if found.
// It is unsafe since it assume that the lock is held.
func (db *MemDB) resolveAggQueriesV2Unsafe() {
	var unresolved []aggQueryV2
	for _, query := range db.aggQueriesV2 {
		if cancelled(query.Cancel) {
			continue // Drop cancelled queries.
		}

		value, ok := db.aggDutiesV2[query.Key]
		if !ok {
			unresolved = append(unresolved, query)
			continue
		}

		query.Response <- value
	}

	db.aggQueriesV2 = unresolved
}

// resolveContribQueriesUnsafe resolves any contribQuery to a result if found.
// It is unsafe since it assumes that the lock is held.
func (db *MemDB) resolveContribQueriesUnsafe() {
	var unresolved []contribQuery
	for _, query := range db.contribQueries {
		if cancelled(query.Cancel) {
			continue // Drop cancelled queries.
		}

		contribution, ok := db.contribDuties[query.Key]
		if !ok {
			unresolved = append(unresolved, query)
			continue
		}

		query.Response <- contribution
	}

	db.contribQueries = unresolved
}

// deleteDutyUnsafe deletes the duty from the database. It is unsafe since it assumes the lock is held.
func (db *MemDB) deleteDutyUnsafe(duty core.Duty) error {
	switch duty.Type {
	case core.DutyProposer:
		delete(db.proDuties, duty.Slot)
	case core.DutyBuilderProposer:
		return core.ErrDeprecatedDutyBuilderProposer
	case core.DutyAttester:
		for _, key := range db.attKeysBySlot[duty.Slot] {
			delete(db.attPubKeys, key)
			delete(db.attDuties, attKey{Slot: key.Slot, CommIdx: key.CommIdx})
		}
		for _, key := range db.attV2KeysBySlot[duty.Slot] {
			delete(db.attV2PubKeys, key)
		}
		delete(db.attKeysBySlot, duty.Slot)
	case core.DutyAggregator:
		for _, key := range db.aggKeysBySlot[duty.Slot] {
			delete(db.aggDuties, key)
			delete(db.aggDutiesV2, key)
		}
		delete(db.aggKeysBySlot, duty.Slot)
	case core.DutySyncContribution:
		for _, key := range db.contribKeysBySlot[duty.Slot] {
			delete(db.contribDuties, key)
		}
		delete(db.contribKeysBySlot, duty.Slot)
	default:
		return errors.New("unknown duty type")
	}

	return nil
}

// attKey is the key to lookup an attester value in the DB.
type attKey struct {
	Slot    uint64
	CommIdx uint64
}

// pkKey is the key to lookup pubkeys by attestation in the DB.
type pkKey struct {
	Slot       uint64
	CommIdx    uint64
	ValCommIdx uint64
}

// pkKey is the key to lookup pubkeys by v2 attestation in the DB.
type pkKeyV2 struct {
	Slot    uint64
	CommIdx uint64
	ValIdx  uint64
}

// aggKey is the key to lookup an aggregated attestation by root in the DB.
type aggKey struct {
	Slot uint64
	Root eth2p0.Root
}

// contribKey is the key to look up sync contribution by root and subcommittee index in the DB.
type contribKey struct {
	Slot       uint64
	SubcommIdx uint64
	Root       eth2p0.Root
}

// attQuery is a waiting attQuery with a response channel.
type attQuery struct {
	Key      attKey
	Response chan<- *eth2p0.AttestationData
	Cancel   <-chan struct{}
}

// proQuery is a waiting proQuery with a response channel.
type proQuery struct {
	Key      uint64
	Response chan<- *eth2api.VersionedProposal
	Cancel   <-chan struct{}
}

// aggQuery is a waiting aggQuery with a response channel.
type aggQuery struct {
	Key      aggKey
	Response chan<- core.AggregatedAttestation
	Cancel   <-chan struct{}
}

// aggQueryV2 is a waiting aggQueryV2 with a response channel.
type aggQueryV2 struct {
	Key      aggKey
	Response chan<- core.VersionedAggregatedAttestation
	Cancel   <-chan struct{}
}

// contribQuery is a waiting contribQuery with a response channel.
type contribQuery struct {
	Key      contribKey
	Response chan<- *altair.SyncCommitteeContribution
	Cancel   <-chan struct{}
}

// cancelled returns true if channel has been closed.
func cancelled(cancel <-chan struct{}) bool {
	select {
	case <-cancel:
		return true
	default:
		return false
	}
}
