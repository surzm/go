package expingest

import (
	"context"
	"database/sql"
	"math/rand"
	"sort"
	"time"

	"github.com/stellar/go/exp/orderbook"
	"github.com/stellar/go/services/horizon/internal/db2/history"
	"github.com/stellar/go/support/errors"
	"github.com/stellar/go/xdr"
)

const (
	verificationFrequency = time.Hour
	updateFrequency       = 2 * time.Second
)

// OrderBookStream updates an in memory graph to be consistent with
// offers in the Horizon DB. Any offers which are created, modified, or removed
// from the Horizon DB during ingestion will be applied to the in memory order book
// graph. OrderBookStream assumes that no other component will update the
// in memory graph. However, it is safe for other go routines to use the
// in memory graph for read operations.
type OrderBookStream struct {
	OrderBookGraph orderbook.OBGraph
	HistoryQ       history.IngestionQ
	lastLedger     uint32
	lastUpdate     time.Time
}

type ingestionStatus struct {
	HistoryConsistentWithState bool
	StateInvalid               bool
	LastIngestedLedger         uint32
	LastOfferCompactionLedger  uint32
}

func (o *OrderBookStream) getIngestionStatus() (ingestionStatus, error) {
	var status ingestionStatus
	var err error

	status.StateInvalid, err = o.HistoryQ.GetExpStateInvalid()
	if err != nil {
		return status, errors.Wrap(err, "Error from GetExpStateInvalid")
	}

	var lastHistoryLedger uint32
	lastHistoryLedger, err = o.HistoryQ.GetLatestLedger()
	if err != nil {
		return status, errors.Wrap(err, "Error from GetLatestLedger")
	}
	status.LastIngestedLedger, err = o.HistoryQ.GetLastLedgerExpIngestNonBlocking()
	if err != nil {
		return status, errors.Wrap(err, "Error from GetLastLedgerExpIngestNonBlocking")
	}
	status.LastOfferCompactionLedger, err = o.HistoryQ.GetOfferCompactionSequence()
	if err != nil {
		return status, errors.Wrap(err, "Error from GetOfferCompactionSequence")
	}

	status.HistoryConsistentWithState = (status.LastIngestedLedger == lastHistoryLedger) ||
		// Running ingestion on an empty DB is a special case because we first ingest from the history archive.
		// Then, on the next iteration, we ingest TX Meta from Stellar Core. So there is a brief
		// period where there will not be any rows in the history_ledgers table but that is ok.
		(lastHistoryLedger == 0)
	return status, nil
}

func addOfferToGraph(graph orderbook.OBGraph, offer history.Offer) {
	sellerID := xdr.MustAddress(offer.SellerID)
	graph.AddOffer(xdr.OfferEntry{
		SellerId: sellerID,
		OfferId:  offer.OfferID,
		Selling:  offer.SellingAsset,
		Buying:   offer.BuyingAsset,
		Amount:   offer.Amount,
		Price: xdr.Price{
			N: xdr.Int32(offer.Pricen),
			D: xdr.Int32(offer.Priced),
		},
		Flags: xdr.Uint32(offer.Flags),
	})
}

func (o *OrderBookStream) update(status ingestionStatus) error {
	reset := o.lastLedger == 0
	if status.StateInvalid || !status.HistoryConsistentWithState {
		log.WithField("status", status).Warn("ingestion state is invalid")
		reset = true
	} else if status.LastIngestedLedger < o.lastLedger {
		log.WithField("status", status).
			WithField("last_ledger", o.lastLedger).
			Warn("ingestion is behind order book last ledger")
		reset = true
	} else if o.lastLedger > 0 && o.lastLedger < status.LastOfferCompactionLedger {
		log.WithField("status", status).
			WithField("last_ledger", o.lastLedger).
			Warn("order book is behind the last offer compaction ledger")
		reset = true
	}

	if reset {
		o.OrderBookGraph.Clear()
		o.lastLedger = 0

		// wait until offers in horizon db is valid before populating order book graph
		if status.StateInvalid || !status.HistoryConsistentWithState {
			log.WithField("status", status).
				Info("waiting for ingestion to update offers table")
			return nil
		}

		defer o.OrderBookGraph.Discard()

		offers, err := o.HistoryQ.GetAllOffers()
		if err != nil {
			return errors.Wrap(err, "Error from GetAllOffers")
		}

		for _, offer := range offers {
			addOfferToGraph(o.OrderBookGraph, offer)
		}

		if err := o.OrderBookGraph.Apply(status.LastIngestedLedger); err != nil {
			return errors.Wrap(err, "Error applying changes to order book")
		}

		o.lastLedger = status.LastIngestedLedger
		return nil
	}

	if status.LastIngestedLedger == o.lastLedger {
		return nil
	}

	defer o.OrderBookGraph.Discard()

	offers, err := o.HistoryQ.GetUpdatedOffers(o.lastLedger)
	if err != nil {
		return errors.Wrap(err, "Error from GetUpdatedOffers")
	}
	for _, offer := range offers {
		if offer.Deleted {
			o.OrderBookGraph.RemoveOffer(offer.OfferID)
		} else {
			addOfferToGraph(o.OrderBookGraph, offer)
		}
	}

	if err = o.OrderBookGraph.Apply(status.LastIngestedLedger); err != nil {
		return errors.Wrap(err, "Error applying changes to order book")
	}

	o.lastUpdate = time.Now()
	o.lastLedger = status.LastIngestedLedger
	return nil
}

func (o *OrderBookStream) verifyAllOffers() {
	offers := o.OrderBookGraph.Offers()
	ingestionOffers, err := o.HistoryQ.GetAllOffers()
	if err != nil {
		// reset last update so that we retry verification on next update
		o.lastUpdate = time.Now().Add(verificationFrequency * -2)
		log.WithError(err).Info("Could not verify offers because of error from GetAllOffers")
		return
	}

	mismatch := len(offers) != len(ingestionOffers)

	if !mismatch {
		sort.Slice(offers, func(i, j int) bool {
			return offers[i].OfferId < offers[j].OfferId
		})
		sort.Slice(ingestionOffers, func(i, j int) bool {
			return ingestionOffers[i].OfferID < ingestionOffers[j].OfferID
		})

		for i, offerRow := range ingestionOffers {
			offerEntry := offers[i]
			if offerRow.OfferID != offerEntry.OfferId ||
				offerRow.Amount != offerEntry.Amount ||
				offerRow.Priced != int32(offerEntry.Price.D) ||
				offerRow.Pricen != int32(offerEntry.Price.N) ||
				!offerRow.BuyingAsset.Equals(offerEntry.Buying) ||
				!offerRow.SellingAsset.Equals(offerEntry.Selling) ||
				offerRow.SellerID != offerEntry.SellerId.Address() {
				mismatch = true
				break
			}
		}
	}

	if mismatch {
		log.WithField("stream_offers", offers).
			WithField("ingestion_offers", ingestionOffers).
			Error("offers derived from order book stream does not match offers from ingestion")
		// set last ledger to 0 so that we reset on next update
		o.lastLedger = 0
	} else {
		log.Info("order book stream verification succeeded")
	}
}

// Update will query the Horizon DB for offers which have been created, removed, or updated since the
// last time Update() was called. Those changes will then be applied to the in memory order book graph.
// After calling this function, the the in memory order book graph should be consistent with the
// Horizon DB (assuming no error is returned).
func (o *OrderBookStream) Update() error {
	if err := o.HistoryQ.BeginTx(&sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead}); err != nil {
		return errors.Wrap(err, "Error starting repeatable read transaction")
	}
	defer o.HistoryQ.Rollback()

	// add 15 minute jitter so that not all horizon nodes are calling
	// HistoryQ.GetAllOffers at the same time
	jitter := time.Duration(rand.Int63n(int64(15 * time.Minute)))
	requiresVerification := !o.lastUpdate.Equal(time.Time{}) &&
		time.Since(o.lastUpdate) >= verificationFrequency+jitter

	status, err := o.getIngestionStatus()
	if err != nil {
		return errors.Wrap(err, "Error obtaining ingestion status")
	}

	if err := o.update(status); err != nil {
		return errors.Wrap(err, "Error updating")
	}

	if requiresVerification {
		o.verifyAllOffers()
	}
	return nil
}

// Run will call Update() every 30 seconds until the given context is terminated.
func (o *OrderBookStream) Run(ctx context.Context) {
	ticker := time.NewTicker(updateFrequency)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := o.Update(); err != nil {
				log.WithError(err).Error("could not apply updates from order book stream")
			}
		case <-ctx.Done():
			log.Info("shutting down OrderBookStream")
			return
		}
	}
}
