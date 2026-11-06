# 0009: One sign-in method per customer

- Status: Accepted
- Date: 2026-11-06
- Deciders: Owner

## Context
The first build merged accounts when a Google customer proved a phone that already had an account. That made one intercepted code enough to attach a stranger's Google account to someone's account for good. The owner asked for a simpler rule, like other apps use: the way you signed up is the way you log in, and you change it on purpose.

## Options considered
1. Merge on a phone proven by OTP, as first built. Convenient, but risky, and `customer_id` could change under the app.
2. One method per customer, with no merging. The customer changes it on purpose.
3. Several methods at once, added in settings. Flexible, but an account has more ways in, and more to explain.

## Decision
Option 2.

- **One method:** each customer has one `sign_in_method`, stored on `customers`: `phone` or `google`, and `apple` later. It is set at sign-up.
- **Phone customers:** always log in with phone and OTP. An email in the profile is contact details only.
- **Google customers:** always log in with Google. The phone is for delivery, verified at ordering.
- **No merging:** a number held by another account is refused with `phone_already_registered`, and `customer_id` never changes.
- **Lost Google account (owner):** the customer signs in with their verified phone. After a correct code, the reply says the account uses Google, and the app offers to switch. Sending `switch_to_phone` with the same code changes the method.
- **Changing in settings:** `changeSignInMethod`, while signed in, after proving the new method.
- **Every change:** removes the old Google link, ends all sessions, tells the phone, and is recorded in the audit log.
- **Empty accounts:** changing to a Google account held only by an empty account from an earlier mistake removes that account.

**Components (owner):** each sign-in method is a `Method` in `internal/identity`, switched on with `SIGNIN_METHODS`, so one can be attached or detached without touching the others. The one-method rule itself lives in `internal/identity/method.go`, so a later change of flow has one place to start. It is not a setting: a security rule with an off switch means an untested path in production.

## Consequences
See [Known downsides](../engineering/known_downsides.md). This also replaces #48: a customer can now remove an unwanted Google account by switching to phone.
