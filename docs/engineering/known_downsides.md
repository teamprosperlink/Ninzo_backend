# Known downsides: ninzo-backend

Every downside we knowingly accepted, in one place, with what limits it and what would fix it. When a decision creates a downside, add it here and link the decision.

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
| Anyone can ask for codes for a number, and 5 requests block its owner for up to an hour | Any phone | Limits per phone. Network limits are in #39 | **Open** | [Rules, section 10](authentication_rules.md#10-open-points) |
| A stolen access token on a live session can change a phone customer to Google, without a code. That ends the owner's sessions | A phone customer whose token is stolen | It needs a token from a live session. The owner can take the account back with their phone: sign in by OTP and switch. It is audited, and the phone is told once the worker exists | Accepted, and review before launch | [0009](../decisions/0009-one-sign-in-method.md) |
| An access token keeps working for up to 15 minutes after sign-out or a change of method | Everyone | Sensitive endpoints must check status. Changing method and asking for a code already refuse an ended session | Accepted | [Rules, section 4](authentication_rules.md#4-sessions) |

## Ordering
| Downside | Who it affects | What limits it today | Status | Source |
|---|---|---|---|---|
| A Google customer learns what is in reach only after verifying a phone | Google customers | A location check without saving anything would fix it | **Open**, for `v0.3.0` | #34 |

## Repository and setup
| Downside | Who it affects | What limits it today | Status | Source |
|---|---|---|---|---|
| GitHub does not block direct pushes to `main` on the free plan | Everyone | Our process only: PRs with passing checks | Accepted | [0008](../decisions/0008-branch-and-release-setup.md) |
| A real login on Android is unproven until the app reaches staging | The customer app | A real Google login was tested on a local web page | Until staging | #25 |
