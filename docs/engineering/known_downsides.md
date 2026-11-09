# Known downsides: ninzo-backend

What is real and what is a stand-in is in `real_and_stand_ins.md`. Every downside we knowingly accepted is here, in one place, with what limits it and what would fix it. When a decision creates a downside, add it here and link the decision.

**Status:** Accepted means we live with it. Before launch means it must be fixed before real customers. Open means someone still has to decide.

## Sign-in
| Downside | Who it affects | What limits it today | Status | Source |
|---|---|---|---|---|
| A customer taps the wrong sign-in button. If a phone customer taps Google, an empty account appears, and they learn at ordering that the number already has an account | A customer who forgets how they signed up | The message says to log in with phone and OTP. The cart stays in the app. Changing to Google in settings removes the empty account | Accepted | [0009](../decisions/0009-one-sign-in-method.md) |
| Empty accounts from that mistake build up | The database | They hold nothing. Nothing removes them yet, unless the customer later changes to that Google account | Accepted, needs a purge later | [0009](../decisions/0009-one-sign-in-method.md) |
| A Google customer who loses their Google account and never verified a phone cannot get back in | A customer who never ordered | They can have no orders, so they lose nothing and can start again | Accepted | [0009](../decisions/0009-one-sign-in-method.md) |
| Someone with an intercepted or read-out code can switch a Google customer to phone. That gives them the account, ends the owner's sessions and removes the owner's Google | A Google customer | It needs the code from that phone. It is audited, and the phone is told once the worker and real SMS exist | **Before launch:** the warning SMS must work | [0009](../decisions/0009-one-sign-in-method.md) |
| A stranger with an intercepted code can put their Google account on a number that has no account yet. The real owner then cannot sign up with that number | The number's real owner | The owner can sign in with OTP and switch to phone, which removes the stranger's Google and ends their sessions. An ended session cannot undo this. The stranger could only come back with a new code from that phone | **Before launch:** the warning SMS must work | [0009](../decisions/0009-one-sign-in-method.md) |
| The warning SMS for a linked or changed account is queued, but not sent | Everyone above | It waits for the worker and real SMS | **Before launch** | #39, `docs/planning/sms_setup.md` |
| A new owner of a reused phone number reaches the previous owner's account | The previous owner | A phone account has no other check. A Google account is only reached by choosing to switch | **Open** | [Rules, section 10](authentication_rules.md#10-open-points) |
| A rider who also orders must use phone and OTP in the customer app, because their number already has an account | Riders | They can change to Google in settings afterwards | Accepted | [0009](../decisions/0009-one-sign-in-method.md) |
| Detaching Google leaves Google customers unable to use Google | Google customers, if the method is switched off | Those with a verified phone can switch at sign-in. Those without have no orders | Accepted | [0009](../decisions/0009-one-sign-in-method.md) |
| Strangers using many addresses can still reach 20 codes an hour for a number, which blocks its owner for up to an hour, and each code costs money | Any phone | Each address gets only 5 an hour for one number, and 100 in all | Accepted, watch it once real SMS is on | [0010](../decisions/0010-rate-limits-in-memory.md) |
| Rate limits are counted in memory, so a restart resets them, and two replicas would each allow the full limit | Everyone | Each API runs one replica for now. The per-phone limits in Postgres still hold | **Before two replicas:** move to Redis | [0010](../decisions/0010-rate-limits-in-memory.md) |
| Refreshing along one session chain, token after token, is limited only per address, so it can still add up to 120 rows a minute | The database | Old rows are purged once #56 exists | Accepted | [0010](../decisions/0010-rate-limits-in-memory.md) |
| `CLIENT_IP_HEADER` is trusted. If Railway ever passed on a client's value, a caller could dodge the address limits | Everyone | Tested on staging on 2026-11-07: Railway overwrote a forged value. The per-phone, per-token and per-session limits do not depend on it | Accepted, recheck after a change of host | [0010](../decisions/0010-rate-limits-in-memory.md) |
| If an attack fills the limiter's table of 200,000 keys, new callers share one bucket per rule. After 500 codes in that bucket, a real new customer can wait up to an hour | New customers, during an attack | It takes a long flood from many addresses. Callers already counted are not affected, and a restart clears it | Accepted, watch it in the logs | [0010](../decisions/0010-rate-limits-in-memory.md) |
| Many customers sharing one mobile network address share its limits, so a busy area could get `rate_limited` | Customers on shared addresses | The limits are generous, and the app retries after `Retry-After` | Accepted, watch it in the pilot | [0010](../decisions/0010-rate-limits-in-memory.md) |
| A stolen access token on a live session can change a phone customer to Google, without a code. That ends the owner's sessions | A phone customer whose token is stolen | It needs a token from a live session. The owner can take the account back with their phone: sign in by OTP and switch. It is audited, and the phone is told once the worker exists | Accepted, and review before launch | [0009](../decisions/0009-one-sign-in-method.md) |
| An access token keeps working for up to 15 minutes after sign-out or a change of method | Everyone | Sensitive endpoints must check status. Changing method and asking for a code already refuse an ended session | Accepted | [Rules, section 4](authentication_rules.md#4-sessions) |

## Profile and devices
| Downside | Who it affects | What limits it today | Status | Source |
|---|---|---|---|---|
| A device stays registered after sign-out without `removeDevice`, after verifying a phone, or for a blocked or suspended person, so it could get that account's notifications | Anyone sharing or losing a phone | Nothing sends notifications yet | **Before the push worker:** tie devices to sessions, send only to live sessions and active people | #58 |
| Someone who has another person's push token can move it to their own account, which cuts that person's notifications | The token's owner | Push tokens are hard to get. Registering it again takes it back | Accepted | #58 |
| A stored `updateMe` reply holds name, email and phone, and nothing purges stored replies yet | Customers | It is only returned for the same key and body, even after a block or an ended session | **Before launch:** purge in #56 | #56 |

## Riders and stock
| Downside | Who it affects | What limits it today | Status | Source |
|---|---|---|---|---|
| Riders are approved by a script, and documents are marked checked by hand, until the console and DigiLocker | Riders | The script is audited, and refuses suspended riders | Until `v0.9.0` | #70 |
| Ending a shift does not check for an order in progress, because orders do not exist yet | Riders | Nothing can be in progress before `v0.4.0` | **Before orders:** add the check | #70 |
| A suspended rider can end their shift only while their access token lasts, up to 15 minutes, so their scooter can stay assigned | Riders | No suspension flow exists yet | **Before suspension:** suspending ends the shift and frees the scooter | #70 |
| A lot is loaded whole. Lots are test data until the hub APIs | Riders | Lots are made by `task testdata` | Until `v0.8.0` | #70 |

## Ordering
| Downside | Who it affects | What limits it today | Status | Source |
|---|---|---|---|---|
| Reach is straight-line distance from a scooter's staging node, at a placeholder speed, not real roads or the rider's position | Customers | The settings are small, and refuse rather than stretch | **Until v0.5.0 and the road test:** rider GPS and real travel times | #72 |
| A basket can mix items that are on different scooters, while one order comes from one scooter | Customers | Each item shows the best single scooter's quantity. The order checks the whole basket in `v0.4.0` | **In v0.4.0:** createOrder picks one scooter for the whole basket | #72 |
| Probing availability on a grid of pins can reveal roughly where staging nodes are, and riders once GPS is used | Riders | It needs a sign-in, and has the session rate limit | **Before rider GPS in v0.5.0:** coarser answers or a per-person limit | #72 |
| Each live stream reads the database about once a second, and a stream on one API copy is capped at 200 | Database load | Small at pilot size | A notification channel before launch scale | #88 |
| A credit is owed but not yet applied to any later order | Customers | Shown on the order as a remedy | Applying credits with the console | #94 |
| The rider stream sends one event for the latest change; if two changed between looks, `seq` skips and the app fetches its state | Riders | This is the README's rule for a skipped `seq` | Accepted | #88 |
| `location_pings` is one table, not split by day yet, and nothing is deleted | Database size | Fine at pilot volume, and pings are kept in full through testing | Daily partitions before launch | #86 |
| A handover is refused as `rider_not_near` only beyond twice the arrival perimeter, to allow for GPS error | Customers | Audit flags record late and GPS-free handovers | Accepted for the pilot | #86 |
| Moving an order to another scooter locks two scooters' stock in one transaction, so two such moves at once can wait on each other. The database stops one, and the next pass tries again | Customers waiting for a rider | Rare, and the pass retries within seconds | Accepted for the pilot | #84 |
| Availability still counts the stock on a scooter whose rider is busy with another order, so an order can wait for that rider | Customers | One order per rider at pilot size, and the wait deadline still applies | Accepted for the pilot | #84 |
| Two gate changes at the same moment, each moving between the same two scooters in opposite directions, can wait on each other. The database stops one, which gets a retryable 500 | Customers changing gates | Very rare at pilot size, and retrying works | Accepted for the pilot | #80 |
| The free-text reason sent with a cancel is not kept | Support | Tickets carry free text | Accepted until support tools need it | #80 |
| Choosing a scooter locks the stock rows of every scooter it looks at, until the order is saved | Customers ordering at once | Pilot size makes the wait tiny | Accepted for the pilot | #78 |
| Availability is checked when asked, not held, so the last unit can go to someone else before ordering | Customers | Orders reserve stock in `v0.4.0` | Accepted | #72 |

## Gates
| Downside | Who it affects | What limits it today | Status | Source |
|---|---|---|---|---|
| A pin on the wrong building inside a served area still counts as verified | Riders and customers | Riders see the entry notes, and support handles a wrong pin | Accepted | [0012](../decisions/0012-gate-verification.md) |
| Gate history keeps labels and pins for good, except for one redaction when a customer is erased | Customers | `RedactGates` removes labels and notes, and blurs pins to about 1 km. No erasure endpoint calls it yet | **Before launch:** build account erasure | #66 |
| The location check can be used to map where we serve | Competitors | It needs a sign-in, and has the session rate limit | Accepted for the pilot | #66 |
| The test zones are rough boxes, not real service areas | Staging | They are marked as test data | Until the owner gives real areas | #66 |

## Repository and setup
| Downside | Who it affects | What limits it today | Status | Source |
|---|---|---|---|---|
| GitHub does not block direct pushes to `main` on the free plan | Everyone | Our process only: PRs with passing checks | Accepted | [0008](../decisions/0008-branch-and-release-setup.md) |
| A real login on Android is unproven until the app reaches staging | The customer app | A real Google login was tested on a local web page | Until staging | #25 |
