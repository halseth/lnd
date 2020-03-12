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

// paymentFailure represents a terminal failure that can happen during the
// payment lifecycle, indicating that the overall payment should be considered
// failed. We use it to record such an encountered error while we wait for the
// remaining shards to finish.
type paymentFailure struct {
	failureCode channeldb.FailureReason
	err         error
}

// Error returns the human-readable error string for the paymentFailure.
func (e *paymentFailure) Error() string {
	return e.err.Error()
}

// shardOutcome represents the parsed result of a payment shard. It can
// indicate if the shard succeeded yielding a preimage, if the shard failed
// with an error that indicates the overall payment should be considered
// failed, or if only the shard failed and we can re-attempt with the remaining
// value.
type shardOutcome struct {
	// preimage is the preimage retrieved from the succeeding shard. Will
	// only be non-zero if neither failures are set.
	preimage lntypes.Preimage

	// paymentFailure indicates this shard encountered a failure serious
	// enough that we should stop attempting this payment and consider it
	// failed. If this is nil it is considered safe to make another shard
	// attempt.
	paymentFailure *paymentFailure

	// shardFailure is an error indicating that the shard failed. If this
	// is non-nil but the paymentFailure is nil, we consider the shard
	// failed, but safe to continue the payment process with new shards.
	shardFailure error
}

// paymentLifecycle holds all information about the current state of a payment
// needed to resume if from any point.
type paymentLifecycle struct {
	router        *ChannelRouter
	totalAmount   lnwire.MilliSatoshi
	paymentHash   lntypes.Hash
	paySession    PaymentSession
	timeoutChan   <-chan time.Time
	currentHeight int32
}

// resumePayment resumes the paymentLifecycle from the current state.
func (p *paymentLifecycle) resumePayment() ([32]byte, *route.Route, error) {
	shardHandler := &shardHandler{
		router:      p.router,
		paymentHash: p.paymentHash,
		quit:        make(chan struct{}),
	}

	// When the payment lifecycle loop exits, we make sure to signal any
	// sub goroutine to exit, then wait for them to return.
	defer func() {
		close(shardHandler.quit)
		shardHandler.wg.Wait()
	}()

	// Each time we send a new payment shard, we'll spin up a goroutine
	// that will collect the result. Either a payment result will be
	// returned, or a critical error signaling that we should immediately
	// exit.
	shardOutcomes := make(chan *shardOutcome)
	criticalErrs := make(chan error)
	var lastError error

	// If we had any existing attempts outstanding, we'll start by and
	// resume spinning up goroutines that'll collect their results and
	// deliver them to the lifecycle loop below.
	existingAttempts, err := p.router.cfg.Control.GetAttempts(
		p.paymentHash,
	)
	if err != nil {
		return [32]byte{}, nil, err
	}

	for _, a := range existingAttempts {
		attempt := a

		shardHandler.wg.Add(1)
		go func() {
			defer shardHandler.wg.Done()

			outcome, err := shardHandler.collectResult(
				&attempt.HTLCAttemptInfo,
			)
			if err != nil {
				select {
				case criticalErrs <- err:
				case <-shardHandler.quit:
				}
				return
			}

			select {
			case shardOutcomes <- outcome:
			case <-shardHandler.quit:
			}
		}()
	}

	var (
		success         = false
		routeFailure    *paymentFailure
		terminalFailure *paymentFailure
	)

	// We'll continue until either our payment succeeds, or we encounter a
	// critical error during path finding.
Loop:
	for {

		// Check if we got any terminal failure.
		var failure *paymentFailure
		switch {
		case terminalFailure != nil:
			failure = terminalFailure

		// We only consider the route failure if we didn't have another
		// terminal failure.
		case routeFailure != nil:
			failure = routeFailure
		}

		// Get all attempts made for this payment, and extract the ones
		// still in flight.
		attempts, err := p.router.cfg.Control.GetAttempts(
			p.paymentHash,
		)
		if err != nil {
			return [32]byte{}, nil, err
		}

		var activeShards int
		var sentAmt lnwire.MilliSatoshi
		for _, a := range attempts {
			if a.Settle != nil || a.Failure != nil {
				continue
			}

			activeShards++
			sentAmt += a.Route.Amt()
		}

		// The payment is finished if we have no outstanding shards,
		// and we recorded either a success or a failure.
		if activeShards == 0 && (success || failure != nil) {
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

		remValue := p.totalAmount - sentAmt

	ShardLoop:
		// While we still have amount to send, and we didn't aleady
		// encounter a success or parmanent failure, send another
		// shard.
		for remValue > 0 && !(success || failure != nil) {
			// Before we attempt this next payment, we'll check to see if either
			// we've gone past the payment attempt timeout, or the router is
			// exiting. In either case, we'll stop this payment attempt short. If a
			// timeout is not applicable, timeoutChan will be nil.
			select {
			case <-p.timeoutChan:
				errStr := fmt.Sprintf("payment attempt not completed " +
					"before timeout")

				terminalFailure = &paymentFailure{
					failureCode: channeldb.FailureReasonTimeout,
					err:         newErr(ErrPaymentAttemptTimeout, errStr),
				}
				break ShardLoop

			// Fall through if we haven't hit our time limit.
			default:
			}

			// Create a new payment attempt from the given payment session.
			rt, err := p.paySession.RequestRoute(
				remValue, uint32(p.currentHeight),
			)
			if err != nil {
				log.Warnf("Failed to find route for payment %x: %v",
					p.paymentHash, err)

				// Convert error to payment-level failure.
				routeFailure = &paymentFailure{
					failureCode: errorToPaymentFailure(err),
					err:         err,
				}

				// If there was an error already recorded for this
				// payment, we'll return that.
				if lastError != nil {
					routeFailure.err = errNoRoute{lastError: lastError}
				}

				break ShardLoop
			}

			// Launch a shard along the route, getting the outcome
			// delivered on the channel.
			shardHandler.launchAndCollect(
				rt, shardOutcomes, criticalErrs,
			)

			activeShards++
			remValue -= rt.Amt()
		}

		// Since no outcome will arrive if there are no active shards,
		// we go back to start to handle the results recorded in the database.
		if activeShards == 0 {
			continue Loop
		}

		// After sending shards covering the total amount, wait for an
		// outcome for one of the shards to be available.
		select {
		case outcome := <-shardOutcomes:
			// We reset the route failure, since now that a result
			// from a previous attempt is back, it might open up
			// for more routes to try.
			routeFailure = nil

			switch {
			// The shard result signals that we should fail the
			// payment.
			case outcome.paymentFailure != nil:
				terminalFailure = outcome.paymentFailure

			// The shard failed, we'll attempt to send another.
			case outcome.shardFailure != nil:
				// Save the forwarding error so it can be
				// returned if this turns out to be the last
				// attempt.
				lastError = outcome.shardFailure

			// The shard succeeded, we'll wait for all shards to
			// finish before we successfully exit the loop.
			default:
				success = true
			}

		// The payment will be resumed from the current state after
		// restart.
		case <-p.router.quit:
			return [32]byte{}, nil, ErrRouterShuttingDown
		}
	}
}

// shardHandler holds what is neccessary to send and collect the result of
// shards.
type shardHandler struct {
	paymentHash lntypes.Hash
	router      *ChannelRouter

	// quit is closed to signal the sub goroutines of the payment lifecycle
	// to stop.
	quit chan struct{}
	wg   sync.WaitGroup
}

// launchAndCollect is an async wrapper around the launch and collect methods,
// that will send a shard along the goven router and deliver the outcome on the
// passed channel.
func (p *shardHandler) launchAndCollect(rt *route.Route,
	outcomeChan chan *shardOutcome, errChan chan error) {

	// We have a new route to try. Launch a shard along this route.
	// Since we'll send any outcome async on the channels, we inspect the
	// outcome and error first in the goroutine below to avoid blocking on
	// the channel send.
	attempt, outcome, launchErr := p.launch(rt)

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()

		// Critical launch error.
		if launchErr != nil {
			select {
			case errChan <- launchErr:
			case <-p.quit:
			}
			return
		}

		// We already have an outcome available, send it and return.
		if outcome != nil {
			select {
			case outcomeChan <- outcome:
			case <-p.quit:
			}
			return
		}

		// Otherwis the attempt was sent successfully, now wait for its
		// result.
		outcome, err := p.collectResult(attempt)
		if err != nil {
			select {
			case errChan <- err:
			case <-p.quit:
			}
			return
		}

		select {
		case outcomeChan <- outcome:
		case <-p.quit:
		}
	}()

}

// launch creates and send a HTLC attempt along the given route. If the HTLC is
// successfully sent an HTLCAttemptInfo will be returned. If we encounter a
// non-critical outcome before we are able to send the HTLC, a non-nil
// shardOutcome will be returned.
func (p *shardHandler) launch(rt *route.Route) (*channeldb.HTLCAttemptInfo,
	*shardOutcome, error) {

	// Using the route received from the payment session, create a new
	// shard to send.
	firstHop, htlcAdd, attempt, err := p.createNewPaymentAttempt(
		rt,
	)
	if err != nil {
		return nil, nil, err
	}

	// Before sending this HTLC to the switch, we checkpoint the fresh
	// paymentID and route to the DB. This lets us know on startup the ID
	// of the payment that we attempted to send, such that we can query the
	// Switch for its whereabouts. The route is needed to handle the result
	// when it eventually comes back.
	err = p.router.cfg.Control.RegisterAttempt(p.paymentHash, attempt)
	if err != nil {
		return nil, nil, err
	}

	// Now that the attempt is created and checkpointed to the DB, we send
	// it.
	sendErr := p.sendPaymentAttempt(attempt, firstHop, htlcAdd)
	if sendErr != nil {
		// TODO(joostjager): Distinguish unexpected internal errors
		// from real send errors.
		err = p.failAttempt(attempt, sendErr)
		if err != nil {
			return nil, nil, err
		}

		// We enncountered an error before we were able to send the
		// shard, return a shardOutcome for the caller to handle this
		// properly.
		outcome := p.handleSendError(attempt, sendErr)
		return nil, outcome, nil
	}

	return attempt, nil, nil
}

// collectResult waits for the result for the given attempt to be available
// from the Switch, then records the attempt outcome with the control tower. A
// shardOutcome is returned, indicating how to continue shard creation for this
// payment.
func (p *shardHandler) collectResult(attempt *channeldb.HTLCAttemptInfo) (
	*shardOutcome, error) {

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
	switch {

	// If this attempt ID is unknown to the Switch, it means it was
	// never checkpointed and forwarded by the switch before a
	// restart. In this case we can safely send a new payment
	// attempt, and wait for its result to be available.
	case err == htlcswitch.ErrPaymentIDNotFound:
		log.Debugf("Payment ID %v for hash %x not found in "+
			"the Switch, retrying.", attempt.AttemptID,
			p.paymentHash)

		cErr := p.failAttempt(attempt, err)
		if cErr != nil {
			return nil, cErr
		}

		return &shardOutcome{
			shardFailure: err,
		}, nil

	// A critical, unexpected error was encountered.
	case err != nil:
		log.Errorf("Failed getting result for attemptID %d "+
			"from switch: %v", attempt.AttemptID, err)

		return nil, err
	}

	// The switch knows about this payment, we'll wait for a result
	// to be available.
	var (
		result *htlcswitch.PaymentResult
		ok     bool
	)

	select {
	case result, ok = <-resultChan:
		if !ok {
			return nil, htlcswitch.ErrSwitchExiting
		}

	case <-p.router.quit:
		return nil, ErrRouterShuttingDown

	case <-p.quit:
		return nil, fmt.Errorf("shard handler exiting")
	}

	// In case of a payment failure, we use the error to decide
	// whether we should retry.
	if result.Error != nil {
		log.Errorf("Attempt to send payment %x failed: %v",
			p.paymentHash, result.Error)

		err = p.failAttempt(attempt, result.Error)
		if err != nil {
			return nil, err
		}

		// Return the parsed outcome.
		outcome := p.handleSendError(attempt, result.Error)
		return outcome, nil
	}

	// We successfully got a payment result back from the switch.
	log.Debugf("Payment %x succeeded with pid=%v",
		p.paymentHash, attempt.AttemptID)

	// Report success to mission control.
	err = p.router.cfg.MissionControl.ReportPaymentSuccess(
		attempt.AttemptID, &attempt.Route,
	)
	if err != nil {
		log.Errorf("Error reporting payment success to mc: %v",
			err)
	}

	// In case of success we atomically store the db payment and
	// move the payment to the success state.
	err = p.router.cfg.Control.SettleAttempt(
		p.paymentHash, attempt.AttemptID,
		&channeldb.HTLCSettleInfo{
			Preimage:   result.Preimage,
			SettleTime: p.router.cfg.Clock.Now(),
		},
	)
	if err != nil {
		log.Errorf("Unable to succeed payment "+
			"attempt: %v", err)
		return nil, err
	}

	// Terminal state, return the outcome wrapping the preimage.
	return &shardOutcome{
		preimage: result.Preimage,
	}, nil
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
func (p *shardHandler) createNewPaymentAttempt(rt *route.Route) (
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
func (p *shardHandler) sendPaymentAttempt(
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

// handleSendError inspects the given error from the Switch and determines
// whether we should make another payment attempt. It returns a shardOutcome
// indicating if the shard failed, or the whole payment should be considered
// failed.
func (p *shardHandler) handleSendError(attempt *channeldb.HTLCAttemptInfo,
	sendErr error) *shardOutcome {

	// The shard failed so set the sendErr as shardFailure to begin with.
	outcome := &shardOutcome{
		shardFailure: sendErr,
	}

	reason := p.router.processSendError(
		attempt.AttemptID, &attempt.Route, sendErr,
	)
	if reason == nil {
		return outcome
	}

	log.Debugf("Payment %x failed: final_outcome=%v, raw_err=%v",
		p.paymentHash, *reason, sendErr)

	// If we find a payment level failure reason, populate the
	// paymentFailure of the outcome to indicate the whole payment should
	// be considered failed, not just the shard.
	outcome.paymentFailure = &paymentFailure{
		failureCode: *reason,
		err:         sendErr,
	}

	return outcome
}

// failAttempt calls control tower to fail the current payment attempt.
func (p *shardHandler) failAttempt(attempt *channeldb.HTLCAttemptInfo,
	sendError error) error {

	failInfo := marshallError(
		sendError,
		p.router.cfg.Clock.Now(),
	)

	return p.router.cfg.Control.FailAttempt(
		p.paymentHash, attempt.AttemptID,
		failInfo,
	)
}

// marshallError marshall an error as received from the switch to a structure
// that is suitable for database storage.
func marshallError(sendError error, time time.Time) *channeldb.HTLCFailInfo {
	response := &channeldb.HTLCFailInfo{
		FailTime: time,
	}

	switch sendError {

	case htlcswitch.ErrPaymentIDNotFound:
		response.Reason = channeldb.HTLCFailInternal
		return response

	case htlcswitch.ErrUnreadableFailureMessage:
		response.Reason = channeldb.HTLCFailUnreadable
		return response
	}

	rtErr, ok := sendError.(htlcswitch.ClearTextError)
	if !ok {
		response.Reason = channeldb.HTLCFailInternal
		return response
	}

	message := rtErr.WireMessage()
	if message != nil {
		response.Reason = channeldb.HTLCFailMessage
		response.Message = message
	} else {
		response.Reason = channeldb.HTLCFailUnknown
	}

	// If the ClearTextError received is a ForwardingError, the error
	// originated from a node along the route, not locally on our outgoing
	// link. We set failureSourceIdx to the index of the node where the
	// failure occurred. If the error is not a ForwardingError, the failure
	// occurred at our node, so we leave the index as 0 to indicate that
	// we failed locally.
	fErr, ok := rtErr.(*htlcswitch.ForwardingError)
	if ok {
		response.FailureSourceIndex = uint32(fErr.FailureSourceIdx)
	}

	return response
}
