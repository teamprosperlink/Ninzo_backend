# SMS setup: MSG91 and DLT

Updated: 2026-11-06 · Owner task, for real SMS

DLT is India's required registration for business SMS. Without it, operators block the SMS. Test OTPs work until this is done, so nothing waits on it. Checked on 2026-11-06, mostly from vendor guides, so confirm fees and menus on the portals.

## A. Register on DLT
On an operator's portal, on a computer, under the company's legal name.

1. Have ready: company PAN, GST or incorporation proof, an address proof, and an authorisation letter naming who signs. The portal lists the exact documents.
2. Pick one portal, such as [Jio TrueConnect](https://trueconnect.jio.com) or Airtel's. One registration is enough.
3. Register as a **Principal Entity**. This cannot be changed later.
4. Pay the one-time fee, about ₹5,900 (₹5,000 + GST). Verification takes about 2-5 working days, possibly longer.
5. Save the **Entity ID**, also called the PE ID.
6. Apply for a **Header (Sender ID)**: 6 letters close to the brand, such as `NINZOA`.
7. Add an **OTP content template**, category OTP or service:
   ```
   {#var#} is your Ninzo login code. It expires in 5 minutes. Do not share it with anyone.
   ```
8. Save the approved **DLT Template ID**.

   Register a second template for phone verification, so the message warns what a code does:
   ```
   {#var#} is your Ninzo code to link this phone to your account. Do not share it with anyone.
   ```

## B. Set up MSG91
After A is approved.

9. Create an account at [msg91.com](https://msg91.com) and add credits. Ask for the current price per SMS, an uptime guarantee in writing, and whether WhatsApp fallback is available.
10. **SMS → Sender ID → Create:** choose India, and enter the approved Header and the **Entity ID**, not the Header ID.
11. **OTP → Templates → Add:** the same wording, with `##OTP##` in place of `{#var#}`, and the DLT Template ID. The text must match the approved template exactly, or SMS fails.
12. Create an **Auth Key**, and restrict it by IP if possible.

## C. What the backend needs
| Item | Secret? | Where it goes |
|---|---|---|
| Sender ID, Entity ID, DLT Template ID, MSG91 Template ID | No | Chat or an Issue |
| MSG91 **Auth Key** | **Yes** | Railway only, never chat, an Issue or a committed file |

**For the build:** send through the outbox and always reply 200, so timing cannot reveal who is registered. See `docs/engineering/authentication_rules.md`, section 5. Errors to know: MSG91 211 means a missing or invalid DLT Template ID, and 203 means a missing PE ID or an unapproved Sender ID.

## Sources
- [MSG91: DLT registration steps](https://msg91.com/guide/steps-for-dlt-process-registration)
- [MSG91: map the Entity ID to the Sender ID](https://msg91.com/help/dlt-registration-in-india/map-your-dlt-entity-pe-id-with-dlt-approved-header-sender-id)
- [MSG91: create an OTP template](https://msg91.com/help/sendotp/where-to-find-the-sendotp-api-how-to-get-template-id)
- [MSG91: DLT debugging checklist](https://msg91.com/help/dlt-registration-in-india/dlt-debugging-checklist)
- [Infobip: DLT registration and fee](https://www.infobip.com/docs/essentials/asia-registration/local-and-international-dlt-registration)
- [EnableX: DLT guide, 2026](https://www.enablex.io/insights/a-step-by-step-guide-to-dlt-registration/)
