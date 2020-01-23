package routing

import (
	"fmt"
	"time"

	"github.com/davecgh/go-spew/spew"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// errNoRoute is returned when all routes from the payment session have been
// attempted.
type errNoRoute struct {
	// lastError is the error encountered during the last payment attempt,
	// if at least one attempt has been made.
	lastError error
}

// Error returns a string representation of the error.
func (e errNoRoute) Error() string {
	return fmt.Sprintf("unable to route payment to destination: %v",
		e.lastError)
}

// paymentShard is a type that wraps an attempt that is part of a (potentially)
// larger payment.
type paymentShard struct {
	*channeldb.HTLCAttemptInfo
}

// paymentShards holds a set of active payment shards.
type paymentShards struct {
	shards     map[uint64]*paymentShard
	totalValue lnwire.MilliSatoshi
}

// addShard adds the given shard to the set of active payment shards.
func (p *paymentShards) addShard(s *paymentShard) {
	if p.shards == nil {
		p.shards = make(map[uint64]*paymentShard)
	}

	// Add the shard and update the total value of the set.
	p.shards[s.AttemptID] = s
	p.totalValue += s.Route.Amt()
}

// removeShard removes the given payment shard from the set.
func (p *paymentShards) removeShard(s *paymentShard) {
	// Remove and updat the total value.
	delete(p.shards, s.AttemptID)
	p.totalValue -= s.Route.Amt()
}

// paymentLifecycle holds all information about the current state of a payment
// needed to resume if from any point.
type paymentLifecycle struct {
	router         *ChannelRouter
	totalAmount    lnwire.MilliSatoshi
	paymentHash    lntypes.Hash
	paySession     PaymentSession
	timeoutChan    <-chan time.Time
	currentHeight  int32
	existingShards *paymentShards
	lastError      error
}

// resumePayment resumes the paymentLifecycle from the current state.
func (p *paymentLifecycle) resumePayment() ([32]byte, *route.Route, error) {
	// The active set of shards start out as existingShards.
	shards := p.existingShards

	// We'll continue until either our payment succeeds, or we encounter a
	// critical error during path finding.
	for {

		// If this payment had no existing payment attempt, we create
		// and send one now.
		if len(shards.shards) == 0 {
			// Before we attempt this next payment, we'll check to see if either
			// we've gone past the payment attempt timeout, or the router is
			// exiting. In either case, we'll stop this payment attempt short. If a
			// timeout is not applicable, timeoutChan will be nil.
			select {
			case <-p.timeoutChan:
				// Mark the payment as failed because of the
				// timeout.
				err := p.router.cfg.Control.Fail(
					p.paymentHash, channeldb.FailureReasonTimeout,
				)
				if err != nil {
					return [32]byte{}, nil, err
				}

				errStr := fmt.Sprintf("payment attempt not completed " +
					"before timeout")

				return [32]byte{}, nil, newErr(ErrPaymentAttemptTimeout, errStr)

			case <-p.router.quit:
				// The payment will be resumed from the current state
				// after restart.
				return [32]byte{}, nil, ErrRouterShuttingDown

			default:
				// Fall through if we haven't hit our time limit, or
				// are expiring.
			}

			// Create a new payment attempt from the given payment session.
			rt, err := p.paySession.RequestRoute(uint32(p.currentHeight))
			if err != nil {
				log.Warnf("Failed to find route for payment %x: %v",
					p.paymentHash, err)

				// Convert error to payment-level failure.
				failure := errorToPaymentFailure(err)

				// If we're unable to successfully make a payment using
				// any of the routes we've found, then mark the payment
				// as permanently failed.
				saveErr := p.router.cfg.Control.Fail(
					p.paymentHash, failure,
				)
				if saveErr != nil {
					return [32]byte{}, nil, saveErr
				}

				// If there was an error already recorded for this
				// payment, we'll return that.
				if p.lastError != nil {
					return [32]byte{}, nil, errNoRoute{lastError: p.lastError}
				}

				// Terminal state, return.
				return [32]byte{}, nil, err
			}

			// Using the route received from the payment session,
			// create a new shard to send.
			firstHop, htlcAdd, attempt, err := p.createNewPaymentAttempt(
				rt,
			)
			// With SendToRoute, it can happen that the route exceeds protocol
			// constraints. Mark the payment as failed with an internal error.
			if err == route.ErrMaxRouteHopsExceeded ||
				err == sphinx.ErrMaxRoutingInfoSizeExceeded {

				log.Debugf("Invalid route provided for payment %x: %v",
					p.paymentHash, err)

				controlErr := p.router.cfg.Control.Fail(
					p.paymentHash, channeldb.FailureReasonError,
				)
				if controlErr != nil {
					return [32]byte{}, nil, controlErr
				}
			}

			// In any case, don't continue if there is an error.
			if err != nil {
				return [32]byte{}, nil, err
			}

			// Before sending this HTLC to the switch, we checkpoint the
			// fresh paymentID and route to the DB. This lets us know on
			// startup the ID of the payment that we attempted to send,
			// such that we can query the Switch for its whereabouts. The
			// route is needed to handle the result when it eventually
			// comes back.
			err = p.router.cfg.Control.RegisterAttempt(p.paymentHash, attempt)
			if err != nil {
				return [32]byte{}, nil, err
			}

			// Now that the attempt was successfully checkpointed
			// to the control tower, add it to our set of active
			// shards.
			s := &paymentShard{
				attempt,
			}
			shards.addShard(s)

			// Now that the attempt is created and checkpointed to
			// the DB, we send it.
			sendErr := p.sendPaymentAttempt(
				s.HTLCAttemptInfo, firstHop, htlcAdd,
			)
			if sendErr != nil {
				// TODO(joostjager): Distinguish unexpected
				// internal errors from real send errors.
				err = p.router.cfg.Control.FailAttempt(
					p.paymentHash,
					s.AttemptID, sendErr,
				)
				if err != nil {
					return [32]byte{}, nil, err
				}

				// We must inspect the error to know whether it
				// was critical or not, to decide whether we
				// should continue trying.
				reason := p.router.processSendError(
					s.AttemptID, &s.Route, sendErr,
				)
				if reason != nil {
					log.Debugf("Payment %x failed: final_outcome=%v, raw_err=%v",
						p.paymentHash, *reason, sendErr)

					// Mark the payment failed with no route.
					//
					// TODO(halseth): make payment codes for the actual reason we don't
					// continue path finding.
					err := p.router.cfg.Control.Fail(
						p.paymentHash, *reason,
					)
					if err != nil {
						return [32]byte{}, nil, err
					}

					// Terminal state, return the error we encountered.
					return [32]byte{}, nil, sendErr
				}
				// Save the forwarding error so it can be returned if
				// this turns out to be the last attempt.
				p.lastError = sendErr

				// Error was handled successfully, remove the
				// shard from our set of active shards indicate
				// we want to make a new attempt.
				shards.removeShard(s)
				continue
			}
		}

		// Temp: get the first (and only) shard.
		var s *paymentShard
		for _, v := range shards.shards {
			s = v
			break
		}

		result, err := p.waitForPaymentResult(s.HTLCAttemptInfo)
		if err != nil {
			log.Errorf("Failed getting result for attemptID %d "+
				"from switch: %v", s.AttemptID, err)

			return [32]byte{}, nil, err
		}

		// In case of a payment failure, we use the error to decide
		// whether we should retry.
		if result.Error != nil {
			log.Errorf("Attempt to send payment %x failed: %v",
				p.paymentHash, result.Error)

			err = p.router.cfg.Control.FailAttempt(
				p.paymentHash, s.AttemptID,
				result.Error,
			)
			if err != nil {
				return [32]byte{}, nil, err
			}

			// We must inspect the error to know whether it was
			// critical or not, to decide whether we should
			// continue trying.
			sendErr := result.Error
			reason := p.router.processSendError(
				s.AttemptID, &s.Route, sendErr,
			)

			if reason != nil {
				log.Debugf("Payment %x failed: final_outcome=%v, raw_err=%v",
					p.paymentHash, *reason, sendErr)

				// Mark the payment failed with no route.
				//
				// TODO(halseth): make payment codes for the actual reason we don't
				// continue path finding.
				err := p.router.cfg.Control.Fail(
					p.paymentHash, *reason,
				)
				if err != nil {
					return [32]byte{}, nil, err
				}

				// Terminal state, return the error we encountered.
				return [32]byte{}, nil, sendErr
			}
			// Save the forwarding error so it can be returned if
			// this turns out to be the last attempt.
			p.lastError = sendErr

			// Error was handled successfully, reset the attempt to
			// indicate we want to make a new attempt.
			shards.removeShard(s)
			continue
		}

		// We successfully got a payment result back from the switch.
		log.Debugf("Payment %x succeeded with pid=%v",
			p.paymentHash, s.AttemptID)

		// Report success to mission control.
		err = p.router.cfg.MissionControl.ReportPaymentSuccess(
			s.AttemptID, &s.Route,
		)
		if err != nil {
			log.Errorf("Error reporting payment success to mc: %v",
				err)
		}

		// In case of success we atomically store the db payment and
		// move the payment to the success state.
		err = p.router.cfg.Control.SettleAttempt(
			p.paymentHash, s.AttemptID,
			result.Preimage,
		)
		if err != nil {
			log.Errorf("Unable to succeed payment "+
				"attempt: %v", err)
			return [32]byte{}, nil, err
		}

		// Terminal state, return the preimage and the route
		// taken.
		return result.Preimage, &s.Route, nil
	}
}

// waitForPaymentResult blocks until a result for the given attempt is returned
// from the switch.
func (p *paymentLifecycle) waitForPaymentResult(
	attempt *channeldb.HTLCAttemptInfo) (*htlcswitch.PaymentResult, error) {

	// If this was a resumed attempt, we must regenerate the
	// circuit. We don't need to check for errors resulting
	// from an invalid route, because the sphinx packet has
	// been successfully generated before.
	_, circuit, err := generateSphinxPacket(
		&attempt.Route, p.paymentHash[:],
		attempt.SessionKey,
	)
	if err != nil {
		return nil, err
	}

	// Using the created circuit, initialize the error decrypter so we can
	// parse+decode any failures incurred by this payment within the
	// switch.
	errorDecryptor := &htlcswitch.SphinxErrorDecrypter{
		OnionErrorDecrypter: sphinx.NewOnionErrorDecrypter(circuit),
	}

	// Now ask the switch to return the result of the payment when
	// available.
	resultChan, err := p.router.cfg.Payer.GetPaymentResult(
		attempt.AttemptID, p.paymentHash, errorDecryptor,
	)
	if err != nil {
		return nil, err
	}

	// The switch knows about this payment, we'll wait for a result
	// to be available.
	select {
	case result, ok := <-resultChan:
		if !ok {
			return nil, htlcswitch.ErrSwitchExiting
		}

		return result, nil

	case <-p.router.quit:
		return nil, ErrRouterShuttingDown
	}
}

// errorToPaymentFailure takes a path finding error and converts it into a
// payment-level failure.
func errorToPaymentFailure(err error) channeldb.FailureReason {
	switch err {
	case
		errNoTlvPayload,
		errNoPaymentAddr,
		errNoPathFound,
		errPrebuiltRouteTried:

		return channeldb.FailureReasonNoRoute

	case errInsufficientBalance:
		return channeldb.FailureReasonInsufficientBalance
	}

	return channeldb.FailureReasonError
}

// createNewPaymentAttempt creates a new payment attempt from the given route.
func (p *paymentLifecycle) createNewPaymentAttempt(rt *route.Route) (
	lnwire.ShortChannelID, *lnwire.UpdateAddHTLC,
	*channeldb.HTLCAttemptInfo, error) {

	// Generate a new key to be used for this attempt.
	sessionKey, err := generateNewSessionKey()
	if err != nil {
		return lnwire.ShortChannelID{}, nil, nil, err
	}

	// Generate the raw encoded sphinx packet to be included along
	// with the htlcAdd message that we send directly to the
	// switch.
	onionBlob, _, err := generateSphinxPacket(
		rt, p.paymentHash[:], sessionKey,
	)
	if err != nil {
		return lnwire.ShortChannelID{}, nil, nil, err
	}

	// Craft an HTLC packet to send to the layer 2 switch. The
	// metadata within this packet will be used to route the
	// payment through the network, starting with the first-hop.
	htlcAdd := &lnwire.UpdateAddHTLC{
		Amount:      rt.TotalAmount,
		Expiry:      rt.TotalTimeLock,
		PaymentHash: p.paymentHash,
	}
	copy(htlcAdd.OnionBlob[:], onionBlob)

	// Attempt to send this payment through the network to complete
	// the payment. If this attempt fails, then we'll continue on
	// to the next available route.
	firstHop := lnwire.NewShortChanIDFromInt(
		rt.Hops[0].ChannelID,
	)

	// We generate a new, unique payment ID that we will use for
	// this HTLC.
	attemptID, err := p.router.cfg.NextPaymentID()
	if err != nil {
		return lnwire.ShortChannelID{}, nil, nil, err
	}

	// We now have all the information needed to populate
	// the current attempt information.
	attempt := &channeldb.HTLCAttemptInfo{
		AttemptID:   attemptID,
		AttemptTime: p.router.cfg.Clock.Now(),
		SessionKey:  sessionKey,
		Route:       *rt,
	}

	return firstHop, htlcAdd, attempt, nil
}

// sendPaymentAttempt attempts to send the current attempt to the switch.
func (p *paymentLifecycle) sendPaymentAttempt(
	attempt *channeldb.HTLCAttemptInfo, firstHop lnwire.ShortChannelID,
	htlcAdd *lnwire.UpdateAddHTLC) error {

	log.Tracef("Attempting to send payment %x (pid=%v), "+
		"using route: %v", p.paymentHash, attempt.AttemptID,
		newLogClosure(func() string {
			return spew.Sdump(attempt.Route)
		}),
	)

	// Send it to the Switch. When this method returns we assume
	// the Switch successfully has persisted the payment attempt,
	// such that we can resume waiting for the result after a
	// restart.
	err := p.router.cfg.Payer.SendHTLC(
		firstHop, attempt.AttemptID, htlcAdd,
	)
	if err != nil {
		log.Errorf("Failed sending attempt %d for payment "+
			"%x to switch: %v", attempt.AttemptID,
			p.paymentHash, err)
		return err
	}

	log.Debugf("Payment %x (pid=%v) successfully sent to switch, route: %v",
		p.paymentHash, attempt.AttemptID, &attempt.Route)

	return nil
}
