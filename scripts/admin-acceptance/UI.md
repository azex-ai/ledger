# Real shadcn UI acceptance

`ui.py` drives the production Next.js consumer in `examples/admin-acceptance`
against its real Go API and a dedicated PostgreSQL database. It does not mock
responses, load personal browser state, or send operator keys to browser code.
Use an isolated acceptance fixture, never a database containing real accounts.

Install Python Playwright and its Chromium in a task-local virtual environment,
then build/start the consumer with `examples/admin-acceptance/stage.py`.

```sh
python scripts/admin-acceptance/ui.py \
  --base-url http://127.0.0.1:43110 \
  --env-file /absolute/private-env.json \
  --psql /absolute/path/to/psql \
  --output /absolute/new-evidence-directory
```

The environment file must have mode 0600 and refer to dedicated loopback API and
PostgreSQL instances. The command uses the seeded holders 1001/1002 as a known
Decimal oracle and compares them with independent SQL entry sums before UI
operations. It creates unique metadata and journals for test holders 990101 and
990102. Only its own new holds on holder 1001 are settled; the seeded hold is
left intact. Successful runs leave configuration/journal evidence in the
acceptance database. Interrupted runs may leave the script's own test holds;
inspect the run's prefix and complete/release those holds or start a fresh
fixture before repeating. The runner never drops a database or deletes journals.

Coverage: all 11 wired pages at 390px; decimal strings at exponents 0/2/6/8/18;
full-value focus/click; independent multi-currency journal totals; classification
ownership; currency validation/create/duplicate/deactivate; mobile template
creation/preview/post/reversal; atomic precision rejection; partial/final/full
settlement without booked debits; account and global reconciliation. API calls
create holds because the present UI has no create-reservation form. This does
not certify optional deposit-review/withdrawal/sweep add-ons, market pricing,
external settlement, every browser, or a production identity provider.

Use a fresh output directory. `results.json` and screenshots retain outcomes;
failed attempts must be kept separately from passing runs. The separate
`role_matrix.py` verifies real BFF/Go authorization; its results do not depend on
buttons being hidden.
