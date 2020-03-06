package routing

import (
	"fmt"
	"sync"
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

// paymentLifecycle holds all information about the current state of a payment
// needed to resume if from any point.
type paymentLifecycle struct {
	router           *ChannelRouter
	totalAmount      lnwire.MilliSatoshi
	paymentHash      lntypes.Hash
	paySession       PaymentSession
	timeoutChan      <-chan time.Time
	currentHeight    int32
	existingAttempts []channeldb.HTLCAttemptInfo
	lastError        error

	// quitCollectShard is closed to signal the collectShard sub goroutines
	// of the payment lifecycle to stop.
	quitCollectShard chan struct{}
	wgCollectShard   sync.WaitGroup
}

// resumePayment resumes the paymentLifecycle from the current state.
func (p *paymentLifecycle) resumePayment() ([32]byte, *route.Route, error) {
	// When the payment lifecycle loop exits, we make sure to signal any
	// sub goroutine to exit, then wait for them to return.
	defer func() {
		close(p.quitCollectShard)
		p.wgCollectShard.Wait()
	}()

	// Cancel the payment session when we exit to signal underlying
	// goroutines can exit.
	defer p.paySession.Cancel()

	// Each time we send a new payment shard, we'll spin up a goroutine
	// that will collect the result. Either a payment result will be
	// returned, or a critical error signaling that we should immediately
	// exit.
	shardResults := make(chan *shardResult)
	criticalErr := make(chan error, 1)
	shards := &PaymentShards{
		PaymentHash: p.paymentHash,
		Control:     p.router.cfg.Control,
	}

	// If we had any existing attempts outstanding, add them to our set of
	// outstanding shards and resume their goroutines such that the final
	// result will be given to the lifecycle loop below.
	for _, a := range p.existingAttempts {
		err := shards.AddShard(
			&a,
			make(chan *RouteResult, 1), // Result will be ignore.
		)
		if err != nil {
			return [32]byte{}, nil, err
		}

		p.wgCollectShard.Add(1)
		go p.collectShard(&a, shardResults, criticalErr)
	}

	type paymentFailure struct {
		failureCode channeldb.FailureReason
		err         error
	}

	var (
		success         = false
		terminalFailure *paymentFailure
		routeFailure    *paymentFailure
	)

	// We'll continue until either our payment succeeds, or we encounter a
	// critical error during path finding.
	for {
		var failure *paymentFailure
		if terminalFailure != nil {
			failure = terminalFailure
		} else if routeFailure != nil {
			failure = routeFailure
		}

		// If we have no outstanding shards, we create and send one
		// now.
		if shards.Num() == 0 && (success || failure != nil) {
			log.Debugf("Payment done")

			// We are done! Get the final attempt results from the
			// database.
			attempts, err := p.router.cfg.Control.GetAttempts(
				p.paymentHash,
			)
			if err != nil {
				log.Errorf("Unable to get payment "+
					"attempts: %v", err)
				return [32]byte{}, nil, err
			}

			// Find the first successful shard and return the
			// preimage and route.
			for _, a := range attempts {
				if a.Settle != nil {
					return a.Settle.Preimage, &a.Route, nil
				}

			}

			// Payment failed, mark it as as permanently failed
			// with the control tower.
			if failure != nil {
				saveErr := p.router.cfg.Control.Fail(
					p.paymentHash, failure.failureCode,
				)
				if saveErr != nil {
					return [32]byte{}, nil, saveErr
				}

				// Terminal state, return.
				return [32]byte{}, nil, failure.err
			}

			return [32]byte{}, nil, fmt.Errorf("No successful " +
				"attempts nor failure found!")
		}

		var (
			newRoute chan *RouteIntent
			routeErr chan error
		)

		// If we are not done and there is still value to be sent,
		// request more routes to send shards along.
		remValue := p.totalAmount - shards.totalValue
		if !success && routeFailure == nil && remValue > 0 {
			log.Debugf("Payment not done, requesting route")

			// When the facts change, I change my mind.
			newRoute, routeErr = p.paySession.RequestRoute(
				remValue, uint32(p.currentHeight),
			)
		}

		// Wait for an exit condition to be reached, or a shard result
		// to be available.
		select {

		// One of the shard goroutines reported a critical error. Exit
		// immediately.
		case err := <-criticalErr:
			return [32]byte{}, nil, err

		// The router is exiting.
		case <-p.router.quit:
			// The payment will be resumed from the current state
			// after restart.
			return [32]byte{}, nil, ErrRouterShuttingDown

		// Before we attempt this next payment, we'll check to see if either
		// we've gone past the payment attempt timeout, or the router is
		// exiting. In either case, we'll stop this payment attempt short. If a
		// timeout is not applicable, timeoutChan will be nil.
		case <-p.timeoutChan:
			errStr := fmt.Sprintf("payment attempt not completed " +
				"before timeout")

			terminalFailure = &paymentFailure{
				failureCode: channeldb.FailureReasonTimeout,
				err:         newErr(ErrPaymentAttemptTimeout, errStr),
			}

		case err := <-routeErr:
			log.Warnf("Failed to find route for payment %x: %v",
				p.paymentHash, err)

			// Convert error to payment-level failure.
			routeFailure = &paymentFailure{
				failureCode: errorToPaymentFailure(err),
				err:         err,
			}

			// If there was an error already recorded for this
			// payment, we'll return that.
			if p.lastError != nil {
				routeFailure.err = errNoRoute{lastError: p.lastError}
			}

		case rt := <-newRoute:
			// Using the route received from the payment session,
			// create a new shard to send.
			firstHop, htlcAdd, attempt, err := p.createNewPaymentAttempt(
				rt.Route,
			)
			// With SendToRoute, it can happen that the route exceeds protocol
			// constraints. Mark the payment as failed with an internal error.
			if err == route.ErrMaxRouteHopsExceeded ||
				err == sphinx.ErrMaxRoutingInfoSizeExceeded {

				log.Debugf("Invalid route provided for payment %x: %v",
					p.paymentHash, err)

				terminalFailure = &paymentFailure{
					failureCode: channeldb.FailureReasonError,
					err:         err,
				}
				break
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
			err = shards.RegisterNewShard(attempt, rt.ResultChan)
			if err != nil {
				return [32]byte{}, nil, err
			}

			// Now that the attempt is created and checkpointed to
			// the DB, we send it.
			sendErr := p.sendPaymentAttempt(
				attempt, firstHop, htlcAdd,
			)
			if sendErr != nil {
				// TODO(joostjager): Distinguish unexpected
				// internal errors from real send errors.
				if err := shards.FailShard(attempt, sendErr); err != nil {
					return [32]byte{}, nil, err
				}

				// We must inspect the error to know whether it
				// was critical or not, to decide whether we
				// should continue trying.
				reason := p.router.processSendError(
					attempt.AttemptID, &attempt.Route, sendErr,
				)
				if reason != nil {
					log.Debugf("Payment %x failed: final_outcome=%v, raw_err=%v",
						p.paymentHash, *reason, sendErr)

					// Mark the payment failed with no route.
					//
					// TODO(halseth): make payment codes for the actual reason we don't
					// continue path finding.
					terminalFailure = &paymentFailure{
						failureCode: *reason,
						err:         sendErr,
					}
					break

				}

				// Save the forwarding error so it can be returned if
				// this turns out to be the last attempt.
				p.lastError = sendErr
				break
			}

			// Now that the shard was sent, spin up a goroutine
			// that will forward the result to the lifecycle loop
			// when available.
			p.wgCollectShard.Add(1)
			go p.collectShard(attempt, shardResults, criticalErr)

		// A result for one of the shards is available.
		case s := <-shardResults:
			// We reset the route failure, since now that a result
			// from a previous attempt is back, it might open up
			// for more routes to try.
			routeFailure = nil

			attempt := s.HTLCAttemptInfo
			result := s.PaymentResult

			// In case of a payment failure, we use the error to decide
			// whether we should retry.
			if result.Error != nil {
				log.Errorf("Attempt to send payment %x failed: %v",
					p.paymentHash, result.Error)

				err := shards.FailShard(attempt, result.Error)
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
					terminalFailure = &paymentFailure{
						failureCode: *reason,
						err:         sendErr,
					}
					break
				}
				// Save the forwarding error so it can be returned if
				// this turns out to be the last attempt.
				p.lastError = sendErr
				break
			}

			// We successfully got a payment result back from the switch.
			log.Debugf("Payment %x succeeded with pid=%v",
				p.paymentHash, s.AttemptID)

			// Report success to mission control.
			err := p.router.cfg.MissionControl.ReportPaymentSuccess(
				s.AttemptID, &s.Route,
			)
			if err != nil {
				log.Errorf("Error reporting payment success to mc: %v",
					err)
			}

			// In case of success we atomically store the db payment and
			// move the payment to the success state.
			if err := shards.SettleShard(attempt, result.Preimage); err != nil {
				log.Errorf("Unable to succeed payment "+
					"attempt: %v", err)
				return [32]byte{}, nil, err
			}

			// Since the assumption is that the whole payment is
			// successful when one shard finishes, mark us done to
			// wait for any outstanding shards.
			success = true
		}
	}
}

// shardResult is a struct that holds the payment result reported from the
// Switch for an attempt we made, together with a reference to the attempt.
type shardResult struct {
	*channeldb.HTLCAttemptInfo
	*htlcswitch.PaymentResult
}

// collectShard waits for a result to be available for the given shard, and
// delivers it on the resultChan.
//
// NOTE: Must be run as a goroutine.
func (p *paymentLifecycle) collectShard(attempt *channeldb.HTLCAttemptInfo,
	resultChan chan<- *shardResult, errChan chan error) {

	defer p.wgCollectShard.Done()

	result, err := p.waitForPaymentResult(attempt)
	if err != nil {
		select {
		case errChan <- err:
		case <-p.quitCollectShard:
		}
		return
	}

	// Notify about the result available for this shard.
	res := &shardResult{
		HTLCAttemptInfo: attempt,
		PaymentResult:   result,
	}

	select {
	case resultChan <- res:
	case <-p.quitCollectShard:
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
