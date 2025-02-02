package control

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/exp/slices"
	"kr.dev/errorfmt"
	"tier.run/refs"
	"tier.run/stripe"
	"tier.run/types/payment"
)

type clockKey struct{}

func WithClock(ctx context.Context, clockID string) context.Context {
	return context.WithValue(ctx, clockKey{}, clockID)
}

func clockFromContext(ctx context.Context) string {
	if v := ctx.Value(clockKey{}); v != nil {
		return v.(string)
	}
	return ""
}

const defaultScheduleName = "default"

// Errors
var (
	ErrOrgNotFound     = errors.New("org not found")
	ErrInvalidMetadata = errors.New("invalid metadata")
	ErrInvalidPhase    = errors.New("invalid phase")
	ErrInvalidCancel   = errors.New("invalid cancel")

	// ErrInvalidFeature is returned when a customer that should have been
	// created is not found after "creating" it. This can happen in Test
	// Mode if the test data was cleared but the idempotency key is still
	// cached at Stripe.
	ErrUnexpectedMissingOrg = errors.New("unexpected missing org")

	errSubscriptionNotFound = errors.New("subscription not found")
)

type ValidationError struct {
	Message string
}

func (e *ValidationError) Error() string { return e.Message }

type InvoiceSettings struct {
	DefaultPaymentMethod string `json:"default_payment_method"`
}

type OrgInfo struct {
	Email       string
	Name        string
	Description string
	Phone       string
	Metadata    map[string]string
	Created     int

	PaymentMethod   string
	InvoiceSettings InvoiceSettings `json:"invoice_settings"`
}

func (oi *OrgInfo) CreatedAt() time.Time {
	return time.Unix(int64(oi.Created), 0)
}

type Phase struct {
	Org       string // set on read
	Effective time.Time
	Features  []refs.FeaturePlan
	Current   bool

	Trial bool // Marks the phase as a trial phase. No fees will be incurred during a trial phase.

	// Plans is the set of plans that are currently active for the phase. A
	// plan is considered active in a phase if all of its features are
	// listed in the phase. If any features from a plan is in the phase
	// without the other features in the plan, this phase is considered
	// "fragmented".
	Plans []refs.Plan

	AutomaticTax bool

	Coupon string // deprecated

	// CouponData is the coupon that was applied to the subscription. It is
	// nil if no coupon was applied.
	CouponData *Coupon
}

// Valid reports if the Phase is one that would be retured from the Stripe API.
// Currently, this is determined if the phase has any features.
func (p *Phase) Valid() bool {
	return len(p.Features) > 0
}

func (p *Phase) Fragments() []refs.FeaturePlan {
	var fs []refs.FeaturePlan
	for _, f := range p.Features {
		if !slices.Contains(p.Plans, f.Plan()) {
			fs = append(fs, f)
		}
	}
	return fs
}

type subscription struct {
	ID           string
	ScheduleID   string
	Status       string
	Name         string
	Effective    time.Time
	TrialEnd     time.Time
	EndDate      time.Time
	CanceledAt   time.Time
	Features     []Feature
	AutomaticTax bool // TODO(bmizerany): cheange to tax.Applied
	Current      Period
	Coupon       *Coupon
}

func (c *Client) lookupSubscription(ctx context.Context, org, name string) (sub subscription, err error) {
	defer errorfmt.Handlef("stripe: lookupSubscription: %s: %s: %w", org, name, &err)
	cid, err := c.WhoIs(ctx, org)
	if err != nil {
		return subscription{}, err
	}

	var f stripe.Form
	f.Set("customer", cid)
	f.Add("expand[]", "data.schedule")
	f.Add("expand[]", "data.discount.coupon")

	type T struct {
		stripe.ID
		Status     string
		StartDate  int64 `json:"start_date"`
		CancelAt   int64 `json:"cancel_at"`
		CanceledAt int64 `json:"canceled_at"`
		TrialEnd   int64 `json:"trial_end"`
		Items      struct {
			Data []struct {
				ID    string
				Price stripePrice
			}
		}
		Metadata struct {
			Name string `json:"tier.subscription"`
		}
		Schedule struct {
			ID string
		}
		AutomaticTax struct {
			Enabled bool
		} `json:"automatic_tax"`
		CurrentPeriodStart int64 `json:"current_period_start"`
		CurrentPeriodEnd   int64 `json:"current_period_end"`
		Discount           struct {
			Coupon stripeCoupon
		}
	}

	// TODO(bmizerany): cache the subscription ID and looked it up
	// specifically on subsequent lookups? It doesn't seem worth it because
	// we can't have more than 50 subscriptions per customer and a Slurp
	// can handle up to 100 since tht is what Stripe allows, so we'll get
	// the subscription in a single call no matter what. The only
	// difference is looping through them wastig a little memory and CPU,
	// but these are rare events (or so we thing ATM) so we can revist
	// if/when we see problems.
	// NOTE: we can't cache the schedule information because it changes
	// over time.

	v, err := stripe.List[T](ctx, c.Stripe, "GET", "/v1/subscriptions", f).Find(func(s T) bool {
		return s.Metadata.Name == name
	})
	if err != nil {
		if errors.Is(err, stripe.ErrNotFound) {
			return subscription{}, errSubscriptionNotFound
		}
		return subscription{}, err
	}

	var fs []Feature
	for _, v := range v.Items.Data {
		f := stripePriceToFeature(v.Price)
		f.ReportID = v.ID
		fs = append(fs, f)
	}

	s := subscription{
		ID:           v.ProviderID(),
		ScheduleID:   v.Schedule.ID,
		Effective:    time.Unix(v.StartDate, 0),
		Status:       v.Status,
		Features:     fs,
		AutomaticTax: v.AutomaticTax.Enabled,
		Current: Period{
			Effective: timeUnix(v.CurrentPeriodStart),
			End:       timeUnix(v.CurrentPeriodEnd),
		},
		Coupon: stripeCouponToCoupon(v.Discount.Coupon),
	}
	if v.TrialEnd > 0 {
		s.TrialEnd = time.Unix(v.TrialEnd, 0)
	}
	if v.CancelAt > 0 {
		s.EndDate = time.Unix(v.CancelAt, 0)
	}
	if v.CanceledAt > 0 {
		s.CanceledAt = time.Unix(v.CanceledAt, 0)
	}
	return s, nil
}

func (c *Client) createSchedule(ctx context.Context, org, name string, fromSub string, p ScheduleParams) (err error) {
	defer errorfmt.Handlef("stripe: createSchedule: %q: %w", org, &err)

	c.Logf("creating schedule for %q from subscription %q: %v", org, fromSub, p.Phases)

	create := func(f stripe.Form) (id string, currentEffective time.Time, err error) {
		var ss struct {
			stripe.ID
			Current struct {
				Start int64 `json:"start_date"`
			} `json:"current_phase"`
		}
		if err := c.Stripe.Do(ctx, "POST", "/v1/subscription_schedules", f, &ss); err != nil {
			return "", time.Time{}, err
		}
		return ss.ProviderID(), timeUnix(ss.Current.Start), nil
	}

	if fromSub != "" {
		defer errorfmt.Handlef("fromSub: %w", &err)
		var f stripe.Form
		f.Set("from_subscription", fromSub)
		id, currentEffective, err := create(f)
		if err != nil {
			return err
		}

		c.Logf("created schedule %q for %q from subscription %q: %v", id, org, fromSub, p.Phases)

		// IMPORTANT: If the caller wants to start the first phase
		// immediately, then we need to update the schedule to set the
		// effective date of the first phase to the current start time
		// of the new first phase.
		if len(p.Phases) > 0 && p.Phases[0].Effective.IsZero() {
			c.Logf("updating schedule %q for %q to set effective date of first phase to %v", id, org, currentEffective)
			p.Phases = slices.Clone(p.Phases) // don't mutate caller's slice
			p.Phases[0].Effective = currentEffective
		}

		// We can only update phases after the schedule is created from
		// the subscription.
		return c.updateSchedule(ctx, id, name, p)
	} else {
		defer errorfmt.Handlef("newSub: %w", &err)
		cid, err := c.WhoIs(ctx, org)
		if err != nil {
			return err
		}
		var f stripe.Form
		if p.PaymentMethod != "" {
			f.Set("default_settings", "default_payment_method", p.PaymentMethod)
		}
		f.Set("customer", cid)
		if err := addPhases(ctx, c, &f, false, name, p.Phases); err != nil {
			return err
		}
		_, _, err = create(f)
		return err
	}
}

type stripeSubSchedule struct {
	stripe.ID
	Current struct {
		Start int64 `json:"start_date"`
		End   int64 `json:"end_date"`
	} `json:"current_phase"`
	Phases []struct {
		Metadata struct {
			Name string `json:"tier.subscription"`
		}
		Start    int64 `json:"start_date"`
		TrialEnd int64 `json:"trial_end"`
		Items    []struct {
			Price stripePrice
		}
		Coupon stripeCoupon
	}
}

func (c *Client) lookupPhases(ctx context.Context, org string, s subscription, name string) (current Phase, all []Phase, err error) {
	defer errorfmt.Handlef("lookupPhases: %w", &err)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	fut := futureGo(func() ([]Feature, error) {
		fs, err := c.Pull(ctx, 0)
		if err != nil {
			return nil, err
		}
		return fs, nil
	})

	wholePlans := func(fs []refs.FeaturePlan) ([]refs.Plan, error) {
		mfs, err := fut.get()
		if err != nil {
			return nil, err
		}
		m := FeaturePlans(mfs)
		var plans []refs.Plan
		for _, f := range fs {
			if slices.Contains(plans, f.Plan()) {
				continue
			}
			inModel := numFeaturesInPlan(m, f.Plan())
			inPhase := numFeaturesInPlan(fs, f.Plan())
			if inModel == inPhase {
				plans = append(plans, f.Plan())
			}
		}
		return plans, nil
	}

	if s.ScheduleID == "" {
		ps := subscriptionToPhases(org, s)
		for i, p := range ps {
			plans, err := wholePlans(p.Features)
			if err != nil {
				return Phase{}, nil, err
			}
			ps[i].Plans = plans
		}
		return ps[0], ps, nil
	}

	var ss stripeSubSchedule
	var f stripe.Form
	f.Add("expand[]", "phases.coupon")
	f.Add("expand[]", "phases.items.price")
	if err := c.Stripe.Do(ctx, "GET", "/v1/subscription_schedules/"+s.ScheduleID, f, &ss); err != nil {
		return Phase{}, nil, err
	}

	m, err := fut.get()
	if err != nil {
		return Phase{}, nil, err
	}
	featureByProviderID := make(map[string]refs.FeaturePlan)
	for _, f := range m {
		featureByProviderID[f.ProviderID] = f.FeaturePlan
	}

	for _, p := range ss.Phases {
		fs := make([]refs.FeaturePlan, 0, len(p.Items))
		for _, pi := range p.Items {
			fs = append(fs, featureByProviderID[pi.Price.ProviderID()])
		}

		plans, err := wholePlans(fs)
		if err != nil {
			return Phase{}, nil, err
		}

		p := Phase{
			Org:       org,
			Effective: time.Unix(p.Start, 0),
			Features:  fs,
			Current:   p.Start == ss.Current.Start,

			Plans: plans,

			Trial: p.TrialEnd > 0,

			AutomaticTax: s.AutomaticTax,

			Coupon:     p.Coupon.ID,
			CouponData: stripeCouponToCoupon(p.Coupon),
		}
		all = append(all, p)
		if p.Current {
			current = p
		}
	}
	slices.SortFunc(all, func(a, b Phase) bool {
		return a.Effective.Before(b.Effective)
	})
	return current, all, nil
}

func subscriptionToPhases(org string, s subscription) []Phase {
	ps := []Phase{{
		Org:       org,
		Effective: s.Effective,
		Features:  FeaturePlans(s.Features),
		Current:   true,
	}}

	if !s.TrialEnd.IsZero() {
		// Break the trial into a separate phase.
		ps = []Phase{{
			Org:       org,
			Effective: s.Effective,
			Features:  FeaturePlans(s.Features),
			Current:   s.Status == "trialing",
			Trial:     true,
		}, {
			Org:       org,
			Effective: s.TrialEnd,
			Features:  FeaturePlans(s.Features),
			Current:   s.Status != "trialing" && s.Status != "canceled",
		}}
	}

	endDate := s.EndDate
	if !s.CanceledAt.IsZero() {
		endDate = s.CanceledAt
	}

	if !endDate.IsZero() {
		ps = append(ps, Phase{
			Org:       org,
			Effective: endDate,
			Features:  nil,
			Current:   s.Status == "canceled",
		})
	}

	for i := range ps {
		if ps[i].Current && s.Coupon != nil {
			ps[i].Coupon = s.Coupon.ID
			ps[i].CouponData = s.Coupon
		}
	}

	return ps
}

func (c *Client) updateSchedule(ctx context.Context, schedID, name string, p ScheduleParams) (err error) {
	defer errorfmt.Handlef("stripe: updateSchedule: %q: %w", schedID, &err)
	if schedID == "" {
		return errors.New("subscription id required")
	}
	var f stripe.Form
	if p.PaymentMethod != "" {
		f.Set("default_settings", "default_payment_method", p.PaymentMethod)
	}
	if err := addPhases(ctx, c, &f, true, name, p.Phases); err != nil {
		return err
	}
	return c.Stripe.Do(ctx, "POST", "/v1/subscription_schedules/"+schedID, f, nil)
}

func (c *Client) cancelSubscription(ctx context.Context, subID string) (err error) {
	defer errorfmt.Handlef("stripe: cancelSchedule: %q: %w", subID, &err)
	if subID == "" {
		return errors.New("subscription id required")
	}
	var f stripe.Form
	// Explicit set to the same as Stripe's defaults to avoid any
	// surprises.
	f.Set("invoice_now", true)
	f.Set("prorate", true)
	return c.Stripe.Do(ctx, "DELETE", "/v1/subscriptions/"+subID, f, nil)
}

func addPhases(ctx context.Context, c *Client, f *stripe.Form, update bool, name string, phases []Phase) error {
	var automaticTax bool
	for i, p := range phases {
		if i > 0 && p.AutomaticTax != automaticTax {
			// TODO(bmizerany): make sentinel error
			return errors.New("stripe: automatic tax must be consistent across phases")
		}
		automaticTax = p.AutomaticTax
		f.Set("default_settings", "automatic_tax", "enabled", automaticTax)

		if len(p.Features) == 0 {
			if i != len(phases)-1 {
				return errors.New("stripe: cancel phase must be the final phase")
			}
			if i == 0 {
				f.Set("phases", i, "end_date", nowOrSpecific(p.Effective))
			} else {
				f.Set("phases", i-1, "end_date", nowOrSpecific(p.Effective))
			}
			f.Set("end_behavior", "cancel")
			return nil
		}

		fs, err := c.lookupFeatures(ctx, p.Features)
		if err != nil {
			return err
		}
		if len(fs) != len(p.Features) {
			return ErrFeatureNotFound
		}

		f.Set("phases", i, "metadata[tier.subscription]", name)
		f.Set("phases", i, "trial", p.Trial)
		if p.Coupon != "" {
			f.Set("phases", i, "coupon", p.Coupon)
		}

		if i == 0 {
			if update {
				f.Set("phases", 0, "start_date", nowOrSpecific(p.Effective))
			} else {
				f.Set("start_date", nowOrSpecific(p.Effective))
			}
		} else {
			f.Set("phases", i-1, "end_date", nowOrSpecific(p.Effective))
			if update {
				f.Set("phases", i, "start_date", nowOrSpecific(p.Effective))
			}
		}
		for j, fe := range fs {
			f.Set("phases", i, "items", j, "price", fe.ProviderID)
		}
	}
	return nil
}

type CheckoutParams struct {
	TrialDays             int
	Features              []Feature
	CancelURL             string
	RequireBillingAddress bool
	AutomaticTax          bool
	CollectTaxID          bool
}

func (c *Client) Checkout(ctx context.Context, org string, successURL string, p *CheckoutParams) (link string, err error) {
	defer errorfmt.Handlef("checkout: %w", &err)

	cid, err := c.putCustomer(ctx, org, nil)
	if err != nil {
		return "", err
	}

	checkout := func(f stripe.Form) (string, error) {
		var v struct{ URL string }
		if err := c.Stripe.Do(ctx, "POST", "/v1/checkout/sessions", f, &v); err != nil {
			return "", err
		}
		return v.URL, nil
	}

	var f stripe.Form
	f.Set("customer", cid)
	f.Set("success_url", successURL)
	f.Set("automatic_tax", "enabled", p.AutomaticTax)
	f.Set("tax_id_collection[enabled]", p.CollectTaxID)
	if p.CancelURL != "" {
		f.Set("cancel_url", p.CancelURL)
	}
	if p.RequireBillingAddress {
		f.Set("billing_address_collection", "required")
	}
	if len(p.Features) == 0 {
		f.Set("mode", "setup")
		// TODO: support other payment methods:
		// https://stripe.com/docs/api/checkout/sessions/create#create_checkout_session-payment_method_types
		f.Set("payment_method_types[]", "card")
		return checkout(f)
	} else {
		f.Set("mode", "subscription")
		f.Set("subscription_data", "metadata", "tier.subscription", "default")
		if p.TrialDays > 0 {
			f.Set("subscription_data", "trial_period_days", p.TrialDays)
		}

		for i, fe := range p.Features {
			f.Set("line_items", i, "price", fe.ProviderID)
			if len(fe.Tiers) == 0 {
				f.Set("line_items", i, "quantity", 1)
			}
		}

		var v struct{ URL string }
		if err := c.Stripe.Do(ctx, "POST", "/v1/checkout/sessions", f, &v); err != nil {
			return "", err
		}
		return v.URL, nil
	}
}

type ScheduleParams struct {
	PaymentMethod string
	Phases        []Phase
}

func (c *Client) Schedule(ctx context.Context, org string, p ScheduleParams) error {
	err := c.schedule(ctx, org, p)
	c.Logf("stripe: schedule: %v", err)
	var e *stripe.Error
	if errors.As(err, &e) {
		if e.Code == "resource_missing" && e.Param == "customer" {
			return ErrUnexpectedMissingOrg
		}
		if strings.Contains(e.Message, "maximum number of items") {
			return ErrTooManyItems
		}
	}
	return err
}

func (c *Client) schedule(ctx context.Context, org string, sp ScheduleParams) (err error) {
	defer errorfmt.Handlef("tier: schedule: %q: %w", org, &err)

	if err := c.PutCustomer(ctx, org, nil); err != nil {
		return err
	}

	if len(sp.Phases) == 0 {
		return errors.New("tier: schedule: at least one phase required")
	}

	scheduleNow := sp.Phases[0].Effective.IsZero()
	cancelNow := scheduleNow && len(sp.Phases[0].Features) == 0

	if cancelNow && len(sp.Phases) > 1 {
		return errors.New("tier: a cancel phase must be the final phase")
	}

	s, err := c.lookupSubscription(ctx, org, defaultScheduleName)
	if errors.Is(err, errSubscriptionNotFound) {
		if cancelNow {
			// No subscription to cancel.
			return nil
		}
		// create a new subscription for org via a schedule, this will
		// also create the org if it does not already exist.
		//
		// If this is a "cancel immediately" request, it returns
		// ErrInvalidCancel because there is no subscription to cancel.
		return c.createSchedule(ctx, org, defaultScheduleName, "", sp)
	}
	if err != nil {
		return err
	}

	if cancelNow {
		return c.cancelSubscription(ctx, s.ID)
	}

	// TODO(bmizerany): check status?

	if s.ScheduleID == "" {
		// We have a subscription, but it is has no active schedule, so start a new one.
		return c.createSchedule(ctx, org, defaultScheduleName, s.ID, sp)
	} else {
		cp, _, err := c.lookupPhases(ctx, org, s, defaultScheduleName)
		if err != nil {
			return err
		}

		if cp.Valid() {
			if scheduleNow {
				// attach phase to current
				sp.Phases[0].Effective = cp.Effective
			} else {
				sp.Phases = append([]Phase{cp}, sp.Phases...)
			}
		}

		err = c.updateSchedule(ctx, s.ScheduleID, defaultScheduleName, sp)
		if isReleased(err) {
			// Lost a race with the clock and the schedule was
			// released just after seeing it, but before our
			// update.
			return c.createSchedule(ctx, org, defaultScheduleName, s.ID, sp)
		}
		if err != nil {
			return err
		}
	}
	return err
}

// isReleased reports if err is the error stripe reports after an attempt to
// update a schedule that is already released.
func isReleased(err error) bool {
	var e *stripe.Error
	if errors.As(err, &e) {
		// Stripe does not return a specific error code for this case
		// so we have to do the brittle thing and check the message.
		return e.Type == "invalid_request_error" && strings.Contains(e.Message, "released")
	}
	return false
}

// SubscribeTo subscribes org to the provided features effective immediately,
// taking over any in-progress schedule. The customer is billed immediately
// with prorations if any.
func (c *Client) SubscribeTo(ctx context.Context, org string, fs []refs.FeaturePlan) error {
	return c.Schedule(ctx, org, ScheduleParams{
		Phases: []Phase{{Features: fs}},
	})

}

func (c *Client) lookupFeatures(ctx context.Context, keys []refs.FeaturePlan) ([]Feature, error) {
	if len(keys) == 0 {
		return nil, errors.New("lookupFeatures: no features provided")
	}

	lookup := func(keys []refs.FeaturePlan) ([]Feature, error) {
		// TODO(bmizerany): return error if len(keys) == 0. No keys means
		// stripe returns all known prices.
		var f stripe.Form
		f.Add("expand[]", "data.tiers")
		for _, k := range keys {
			f.Add("lookup_keys[]", stripe.MakeID(k.String()))
		}
		pp, err := stripe.Slurp[stripePrice](ctx, c.Stripe, "GET", "/v1/prices", f)
		if err != nil {
			return nil, err
		}

		if len(pp) != len(keys) {
			// TODO(bmizerany): report which feature(s) was/are not found
			return nil, ErrFeatureNotFound
		}

		fs := make([]Feature, len(pp))
		for i, p := range pp {
			fs[i] = stripePriceToFeature(p)
		}
		return fs, nil
	}

	var fs []Feature
	// lookup 10 keys at a time
	for len(keys) > 0 {
		n := 10
		if len(keys) < n {
			n = len(keys)
		}
		lfs, err := lookup(keys[:n])
		if err != nil {
			return nil, err
		}
		fs = append(fs, lfs...)
		keys = keys[n:]
	}
	return fs, nil
}

func notFoundAsNil(err error) error {
	if errors.Is(err, stripe.ErrNotFound) ||
		errors.Is(err, ErrOrgNotFound) {
		return nil
	}
	return err
}

func (c *Client) LookupStatus(ctx context.Context, org string) (string, error) {
	s, err := c.lookupSubscription(ctx, org, defaultScheduleName)
	if err != nil {
		return "", err
	}
	return s.Status, nil
}

type Coupon struct {
	ID               string `json:"id"`
	Metadata         map[string]string
	Created          time.Time
	AmountOff        int     `json:"amount_off"`
	Currency         string  `json:"currency"`
	Duration         string  `json:"duration"`
	DurationInMonths int     `json:"duration_in_months"`
	MaxRedemptions   int     `json:"max_redemptions"`
	Name             string  `json:"name"`
	PercentOff       float64 `json:"percent_off"`
	RedeemBy         time.Time
	TimesRedeemed    int  `json:"times_redeemed"`
	Valid            bool `json:"valid"`
}

type stripeCoupon struct {
	Coupon
	Created  int64
	RedeemBy int64 `json:"redeem_by"`
}

func stripeCouponToCoupon(sc stripeCoupon) *Coupon {
	if sc.ID == "" {
		return nil
	}
	c := &sc.Coupon
	c.Created = timeUnix(sc.Created)
	c.RedeemBy = timeUnix(sc.RedeemBy)
	return c
}

type Schedule struct {
	Current Period
	Phases  []Phase
}

func (c *Client) LookupPhases(ctx context.Context, org string) (ps *Schedule, err error) {
	s, err := c.lookupSubscription(ctx, org, defaultScheduleName)
	if errors.Is(err, errSubscriptionNotFound) {
		return &Schedule{}, nil
	}
	if err != nil {
		return nil, err
	}

	_, all, err := c.lookupPhases(ctx, org, s, defaultScheduleName)
	if err != nil {
		return nil, err
	}
	cs := &Schedule{
		Current: s.Current,
		Phases:  all,
	}
	return cs, nil
}

// LookupPaymentMethods returns the payment methods for the given org.
func (c *Client) LookupPaymentMethods(ctx context.Context, org string) ([]payment.Method, error) {
	cid, err := c.WhoIs(ctx, org)
	if err != nil {
		return nil, err
	}
	var f stripe.Form
	return stripe.Slurp[payment.Method](ctx, c.Stripe, "GET", "/v1/customers/"+cid+"/payment_methods", f)
}

type Period struct {
	Effective time.Time
	End       time.Time
}

type Invoice struct {
	Amount float64
	Period Period
	Lines  []InvoiceLineItem

	SubtotalPreTax int
	Subtotal       int
	TotalPreTax    int
	Total          int
}

type InvoiceLineItem struct {
	Period    Period
	Feature   refs.FeaturePlan
	Quantity  int
	Amount    float64
	Proration bool

	// TODO(bmizerany): add more fields
}

type stripeInvoiceLineItem struct {
	Period struct {
		Start int64 `json:"start"`
		End   int64 `json:"end"`
	}

	// Price is unable to collect Tiers because the
	// price object itself is not expandable
	// because it is the 4th level of nesting, and
	// the maximum level of nesting is 4.
	Price        stripePrice
	Amount       float64
	Quantity     int
	AmountPreTax int `json:"amount_excluding_tax"`
	Currency     string
	Proration    bool
}

func (c *Client) LookupInvoices(ctx context.Context, org string) ([]Invoice, error) {
	// /v1/invoices

	// slurp all invoices for org using the code already in this file
	cid, err := c.WhoIs(ctx, org)
	if err != nil {
		return nil, notFoundAsNil(err)
	}

	var f stripe.Form
	f.Set("customer", cid)

	type T struct {
		// https://stripe.com/docs/api/invoices/object
		stripe.ID
		PeriodStart          int64 `json:"period_start"`
		PeriodEnd            int64 `json:"period_end"`
		SubtotalExcludingTax int   `json:"subtotal_excluding_tax"`
		Subtotal             int   `json:"subtotal"`
		TotalExcludingTax    int   `json:"total_excluding_tax"`
		Total                int   `json:"total"`
		Lines                struct {
			Data []stripeInvoiceLineItem
		}
	}

	sins, err := stripe.Slurp[T](ctx, c.Stripe, "GET", "/v1/invoices", f)
	if err != nil {
		return nil, err
	}

	var ins []Invoice
	for _, in := range sins {
		var lines []InvoiceLineItem
		for _, line := range in.Lines.Data {
			lines = append(lines, InvoiceLineItem{
				Period: Period{
					Effective: time.Unix(line.Period.Start, 0),
					End:       time.Unix(line.Period.End, 0),
				},
				Feature:   line.Price.Metadata.Feature,
				Quantity:  line.Quantity,
				Amount:    line.Amount,
				Proration: line.Proration,
			})
		}
		ins = append(ins, Invoice{
			Period: Period{
				Effective: time.Unix(in.PeriodStart, 0),
				End:       time.Unix(in.PeriodEnd, 0),
			},
			Lines:          lines,
			SubtotalPreTax: in.SubtotalExcludingTax,
			Subtotal:       in.Subtotal,
			TotalPreTax:    in.TotalExcludingTax,
			Total:          in.Total,
		})
	}
	return ins, nil
}

// PutCustomer safely creates or updates a customer in Stripe. It does this
// being careful to not duplicate customer records. If the customer already exists, it
// will be updated with the provided info.
func (c *Client) PutCustomer(ctx context.Context, org string, info *OrgInfo) error {
	_, err := c.putCustomer(ctx, org, info)
	var e *stripe.Error
	if errors.As(err, &e) && e.Code == "email_invalid" {
		return ErrInvalidEmail
	}
	return err
}

// putCustomer safely creates a customer in Stripe for the provided org
// identifier if one does not already exists.
//
// It uses an idempotency key to avoid racing with clients that may also be
// attempting to create the same customer. This is necessary because unlike
// products and prices, customer records do not have unique user-defined
// identifiers, so we have to first look if a record already exists, and
// subsequently create one if it doesn't, but being careful to not race with
// another client which may have also seen there was no record and is attempting
// to create the record at the same time we are.  It returns the empty string
// without an error if no customer exists with the provided email.
//
// It only returns errors encountered while communicating with Stripe.
func (c *Client) putCustomer(ctx context.Context, org string, info *OrgInfo) (string, error) {
	cid, err := c.WhoIs(ctx, org)
	if errors.Is(err, ErrOrgNotFound) {
		return c.createCustomer(ctx, org, info)
	}
	if err != nil {
		return "", err
	}

	// finally, update the already created customer
	if err := c.updateCustomer(ctx, cid, info); err != nil {
		return "", err
	}
	return cid, nil
}

type Account struct {
	ProviderID string `json:"id"`
	Email      string `json:"email"`
	CreatedAt  int64  `json:"created"`
	KeySource  string `json:"key_source"`
	Isolated   bool   `json:"isolated"`
}

func (a *Account) Created() time.Time {
	return time.Unix(a.CreatedAt, 0)
}

func (a *Account) URL() string {
	return fmt.Sprintf("https://dashboard.stripe.com/%s", a.ProviderID)
}

func (c *Client) WhoAmI(ctx context.Context) (Account, error) {
	var a Account
	var f stripe.Form
	if err := c.Stripe.Do(ctx, "GET", "/v1/account", f, &a); err != nil {
		return Account{}, err
	}
	a.Isolated = c.Stripe.AccountID != ""
	a.KeySource = c.KeySource
	return a, nil
}

func (c *Client) Isolated() bool {
	return c.Stripe.AccountID != ""
}

type stripeCustomer struct {
	stripe.ID
	Email    string
	Metadata struct {
		Org string `json:"tier.org"`
	}
}

func (c *Client) WhoIs(ctx context.Context, org string) (id string, err error) {
	defer errorfmt.Handlef("whois: %q: %w", org, &err)
	if !strings.HasPrefix(org, "org:") {
		return "", &ValidationError{Message: "org must be prefixed with \"org:\""}
	}

	clockID := clockFromContext(ctx)

	key := orgKey{
		name:    org,
		account: c.Stripe.AccountID,
		clock:   clockID,
	}

	cid, err := c.cache.load(key, func() (string, error) {
		var f stripe.Form
		if clockID != "" {
			f.Set("test_clock", clockID)
		}
		cus, err := stripe.List[stripeCustomer](ctx, c.Stripe, "GET", "/v1/customers", f).
			Find(func(v stripeCustomer) bool {
				return v.Metadata.Org == org
			})
		if err != nil {
			return "", err
		}
		return cus.ProviderID(), nil
	})
	if errors.Is(err, stripe.ErrNotFound) {
		return "", ErrOrgNotFound
	}
	return cid, err
}

// LookupOrg returns the org information on file with Stripe, uncached.
func (c *Client) LookupOrg(ctx context.Context, org string) (*OrgInfo, error) {
	cid, err := c.WhoIs(ctx, org)
	if err != nil {
		return nil, err
	}
	var f stripe.Form
	var info *OrgInfo
	if err := c.Stripe.Do(ctx, "GET", "/v1/customers/"+cid, f, &info); err != nil {
		return nil, err
	}

	for k := range info.Metadata {
		if strings.HasPrefix(k, "tier.") {
			delete(info.Metadata, k)
		}
	}

	return info, nil
}

func (c *Client) createCustomer(ctx context.Context, org string, info *OrgInfo) (id string, err error) {
	defer errorfmt.Handlef("createCustomer: %w", &err)

	clockID := clockFromContext(ctx)
	key := orgKey{
		account: c.Stripe.AccountID,
		clock:   clockID,
		name:    org,
	}

	return c.cache.load(key, func() (string, error) {
		var f stripe.Form

		var b strings.Builder
		b.WriteString("customer:create:")
		b.WriteString(key.name)
		b.WriteString(key.account)
		b.WriteString(key.clock)
		f.SetIdempotencyKey(b.String())

		f.Set("metadata[tier.org]", org)
		if err := setOrgInfo(&f, info); err != nil {
			return "", err
		}
		if clockID != "" {
			f.Set("test_clock", clockID)
		}
		var created struct {
			stripe.ID
		}
		if err := c.Stripe.Do(ctx, "POST", "/v1/customers", f, &created); err != nil {
			return "", err
		}
		return created.ProviderID(), nil
	})
}

func setOrgInfo(f *stripe.Form, info *OrgInfo) error {
	if info == nil {
		return nil
	}
	stripe.MaybeSet(f, "email", info.Email)
	stripe.MaybeSet(f, "name", info.Name)
	stripe.MaybeSet(f, "phone", info.Phone)
	stripe.MaybeSet(f, "description", info.Description)
	stripe.MaybeSet(f, "payment_method", info.PaymentMethod)
	stripe.MaybeSet(f, "invoice_settings[default_payment_method]", info.InvoiceSettings.DefaultPaymentMethod)
	for k, v := range info.Metadata {
		if strings.HasPrefix(k, "tier.") {
			return fmt.Errorf("%w: %q", ErrInvalidMetadata, k)
		}
		f.Set("metadata", k, v)
	}
	return nil
}

func (c *Client) updateCustomer(ctx context.Context, id string, info *OrgInfo) error {
	if info == nil {
		return nil
	}
	// update customer in stripe
	var f stripe.Form
	if err := setOrgInfo(&f, info); err != nil {
		return err
	}
	return c.Stripe.Do(ctx, "POST", "/v1/customers/"+id, f, nil)
}

func nowOrSpecific(t time.Time) any {
	if t.IsZero() {
		return "now"
	}
	return t
}

func numFeaturesInPlan(fs []refs.FeaturePlan, plan refs.Plan) (n int) {
	for _, f := range fs {
		if f.InPlan(plan) {
			n++
		}
	}
	return n
}

// timeUnix is like time.Unix, but returns the zero time if n is zero.
func timeUnix(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

type future[T any] struct {
	ch  chan struct{}
	v   T
	err error
}

func futureGo[T any](fn func() (T, error)) *future[T] {
	f := &future[T]{ch: make(chan struct{})}
	go func() {
		f.v, f.err = fn()
		close(f.ch)
	}()
	return f
}

func (f *future[T]) get() (T, error) {
	<-f.ch
	return f.v, f.err
}
