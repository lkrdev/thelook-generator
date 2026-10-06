# TheLook Data Generator Specification (SPEC.md)

This document records the design rules, statistical distributions, customer cohorts, and generation behaviors established for TheLook synthetic e-commerce data generator.

## 1. Generation Philosophy & Architectural Invariants

- Deterministic Time Invariance: Generation volume and growth factors at any timestamp are pure deterministic functions of time. Backfill simulation does not need to precede live data generation. A simulation running in 2017, 2020, or live in 2026 computes its exact volume factor in O(1) time with identical, seamless continuity.
- Organic Non-Linearity: Data reflects authentic business messiness rather than flat synthetic repetitions. Different years have distinct curves, unpredictable breakout months, seasonal shopping valleys, and customer segments with highly skewed purchasing power.
- Referential Integrity: Web events, orders, inventory movements, transactions, and ad events strictly obey relational causal order. Users must register before purchasing, browse products before carting, and stock inventory before fulfilling orders.

## 2. Store Growth Dynamics & Monthly Trajectories

### Within-Year Growth Dynamics
- Month-over-Month Floor: MoM growth within a calendar year is capped on the downside; monthly volume never drops by more than 5% (floor at -5%).
- Average MoM Growth: Outside of seasonal shocks, monthly volume advances with an expected average MoM increase of 10%.
- Random Annual Hyper-Growth Month: Exactly one month per calendar year is randomly selected (between February and October) to experience a +20% hyper-growth surge.
- Unique Year Profiles: Because the hyper-growth month and monthly noise are seeded per calendar year, each year on a "month-by-year" Looker chart displays a unique, non-parallel shape rather than identical stacked lines.

### Year-over-Year Compounding
- Annual Scaling: The baseline store volume scales organically year-over-year (~25% to 35% annual compounding), taking the store from an early startup phase in 2016 (~100k events/month) to an established multi-million event enterprise by 2026.
- Database Upper Bound: Long-term compounding is moderated at maturity to keep database ingest rates within practical VM and streaming limits (~5x to 8x total expansion over a decade).

## 3. Customer Segmentation & User Cohorts

The user base is partitioned into three distinct behavioral cohorts established upon customer registration:

### Power Buyers (~5% of Users)
- Business Impact: Account for 15% to 20% of all completed orders and transactions.
- Shopping Behavior: High purchase urgency with elevated cart-to-purchase conversion (~75%).
- Retention: Return to the site frequently, making up ~16% of returning browsing sessions.

### Heavy Browsers / Window Shoppers (~1% of Users)
- Business Impact: Account for ~10% of total web traffic and pageview events, but less than 1% of completed orders.
- Shopping Behavior: Deep, multi-category window shopping sessions with very low purchase conversion (< 3% cart rate).
- Purpose: Produces realistic e-commerce traffic volume, high engagement analytics, and long-tail session metrics without distorting revenue or order fulfillment tables.

### Standard Customers (~94% of Users)
- Business Impact: Account for the remaining ~80% of orders and ~75% of web traffic.
- Shopping Behavior: Normal retail behavior with moderate browsing depth and standard conversion rates (~55% cart rate, 85% checkout completion).

## 4. Session Dynamics & Funnel Behavior

### Exponential Session Length Distribution
- Median at 5 Events: Session depth follows an exponential decay distribution with the population median set to 5 pageviews.
- Elimination of Hard Cutoffs: Replaces rigid 3-to-5 event fixed sequences. Sequence numbers decay smoothly from 1 through 20+, providing an authentic power-law distribution in funnel depth reports.
- User Session Length Personality: Different users exhibit inherent browsing depth tendencies. Quick searchers have shorter medians (~3 events), while deep explorers have longer medians (~7 events), independent of overall visit frequency.

### Funnel Progression Rules
- Homepage / Entry: Sessions initiate on top-level navigation (`/home`, `/department/...`, or marketing landing pages).
- Multi-Page Navigation: Deeper sessions alternate organically between department listings, brand collections, and individual product detail views.
- Mandatory Product Precondition: A session must view a product page detail (`/product/:id`) prior to emitting an add-to-cart event (`/cart`).
- Cart Urgency: Shoppers choose between instant checkout within seconds or deferred multi-session return checkout hours or days later.

## 5. Calendar Seasonality & Shopping Rhythms

### Diurnal (Hourly) Rhythm
- Early morning hours (02:00 to 06:00 local) experience trough traffic.
- Traffic climbs steadily during business hours and peaks in the late afternoon and evening (14:00 to 21:00).

### Day of Week (Weekly) Cycle
- Sunday and Monday drive peak online retail browsing and checkout.
- Friday and Saturday drop in browsing volume as consumer screen time declines.

### Major Retail Holidays
- Cyber Week / Peak Holiday: Traffic surges to 2.5x baseline from late November through December 23, accompanied by elevated discount frequency (30%+) and deeper markdown tiers.
- Post-Christmas Valley: Sharp drop in traffic (0.65x baseline) from December 26 through mid-January, combined with high clearance markdown frequency.
- Promotional Holiday Weekends: Memorial Day and Labor Day weekends produce 1.35x volume lifts driven by aggressive promotional discounting.
- Fourth of July: 30% drop in online retail activity due to summer travel and outdoor holidays.

### Category Seasonality
- Warm Weather Apparel (Swimwear, Shorts, Dresses): 2.5x demand surge between May and August; 75% drop between November and February.
- Cold Weather Apparel (Coats, Sweaters, Fleece, Outerwear): 2.5x demand surge between October and February; 75% drop between May and August.
- Everyday Basics (Underwear, Socks, Jeans): Constant year-round demand.

## 6. Order Fulfillment & Post-Holiday Logistics

### Cancellations
- Approximately 1% to 2% of placed orders are cancelled within hours of checkout.

### Warehouse & Shipping Delays
- Standard Handling: Orders ship within 1 to 3 days under normal operational conditions.
- Holiday Shipping Congestion: Between November 20 and December 24, warehouse handling and carrier backlogs extend delivery transit times to 4 to 9 days.

### Returns & Holiday Surge
- Standard Return Rate: Baseline return rate is ~1.1% throughout normal months.
- Holiday Gift Return Surge: Orders placed during late November and December experience a sharp return spike in January, rising to ~6.5% as gift recipients exchange or return unwanted items.
