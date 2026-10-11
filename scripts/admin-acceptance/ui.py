#!/usr/bin/env python3
"""Real shadcn consumer acceptance. Requires the isolated admin-acceptance seed.

Uses actual browser controls for management; HTTP is used for independent state
checks and creating reservation fixtures (the UI has no create-hold form).
Credentials are read from a 0600 file and never logged. No mocked API traffic.
"""
import argparse
import json
import re
import secrets
import stat
import subprocess
from decimal import Decimal
from pathlib import Path
from urllib.error import HTTPError
from urllib.parse import urlsplit
from urllib.request import Request, urlopen

from playwright.sync_api import expect, sync_playwright


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--base-url', required=True)
    parser.add_argument('--env-file', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--psql', default='psql')
    args = parser.parse_args()
    base = args.base_url.rstrip('/')
    if urlsplit(base).hostname != '127.0.0.1':
        raise SystemExit('Only isolated loopback hosts are supported')
    if stat.S_IMODE(args.env_file.stat().st_mode) & 0o077:
        raise SystemExit('Private env must have mode 0600 or stricter')
    env = json.loads(args.env_file.read_text())
    api = env['LEDGER_API_URL_INTERNAL']
    if urlsplit(api).hostname != '127.0.0.1' or urlsplit(env['DATABASE_URL']).hostname != '127.0.0.1':
        raise SystemExit('API and database must also be dedicated loopback resources')
    args.output.mkdir(parents=True, exist_ok=True)
    if (args.output / 'results.json').exists():
        raise SystemExit('Use a fresh output directory to retain earlier evidence')
    prefix = 'UI_' + secrets.token_hex(4).upper()
    checks, errors = [], []

    def check(name, passed, **evidence):
        row = {'case': name, 'pass': bool(passed), **evidence}
        checks.append(row)
        print(json.dumps(row), flush=True)
        assert passed, name

    def http(method, path, body=None, expected=200):
        headers = {'Authorization': 'Bearer ' + env['LEDGER_KEY_ADMIN']}
        if body is not None:
            headers['Content-Type'] = 'application/json'
        req = Request(api + '/api/v1/' + path, headers=headers, method=method,
                      data=None if body is None else json.dumps(body).encode())
        try:
            response = urlopen(req, timeout=20)
        except HTTPError as error:
            response = error
        with response:
            data = json.load(response)
            assert response.status == expected, (path, response.status, data)
            return data.get('data')

    def rows(path):
        return http('GET', path)['list']

    currencies = {c['code']: c for c in rows('currencies?active_only=false')}
    expected_amounts = {'USD': '1234.56', 'USDC': '1.234567', 'BTC': '0.00000001',
                        'POINTS': '42', 'ATOM': '999999999999.123456789012345678'}
    assert set(expected_amounts).issubset(currencies), 'Acceptance seed is required'
    usd = currencies['USD']['uid']
    sql = '''SELECT coalesce(json_agg(t),'[]') FROM (
      SELECT e.account_holder,c.code,sum(CASE WHEN e.entry_type=cl.normal_side
        THEN e.amount ELSE -e.amount END)::text AS booked
      FROM journal_entries e JOIN currencies c ON c.id=e.currency_id
      JOIN classifications cl ON cl.id=e.classification_id
      WHERE e.account_holder IN (1001,1002) GROUP BY e.account_holder,c.code
      ORDER BY e.account_holder,c.code) t;'''
    sql_result = subprocess.run([args.psql, env['DATABASE_URL'], '-XAt', '-c', sql],
                                capture_output=True, text=True, check=True)
    sql_rows = json.loads(sql_result.stdout)
    for code, amount in expected_amounts.items():
        db = next(r['booked'] for r in sql_rows if r['account_holder'] == 1001 and r['code'] == code)
        wire = rows('balances/1001?currency_uid=' + currencies[code]['uid'])
        check('decimal_oracle_' + code, Decimal(db) == Decimal(amount) and
              len(wire) == 1 and isinstance(wire[0]['balance'], str) and Decimal(wire[0]['balance']) == Decimal(amount),
              expected=amount, sql=db, api=wire[0]['balance'])
    def breakdown():
        return http('GET', 'balances/1001/' + usd + '/breakdown')
    before = breakdown()
    check('booked_held_available_seed', Decimal(before['total']) == Decimal('1234.56') and
          Decimal(before['locked']) == Decimal('34.56') and Decimal(before['available']) == Decimal('1200'))
    pending = [r for r in rows('bookings?holder=1001') if r['status'] == 'pending']
    check('pending_deposit_not_credited', any(Decimal(r['amount']) == Decimal('5.000001') for r in pending))

    with sync_playwright() as p:
        browser = p.chromium.launch()
        context = browser.new_context(viewport={'width': 1440, 'height': 1000})
        page = context.new_page()
        page.set_default_timeout(15000)
        page.on('pageerror', lambda error: errors.append(str(error)))

        def save(name):
            page.screenshot(path=str(args.output / (name + '.png')), full_page=True)

        def nav(name):
            page.get_by_role('link', name=name, exact=True).click()
            expect(page.get_by_role('heading', name=name, exact=True)).to_be_visible()
            expect(page.locator('main .animate-shimmer')).to_have_count(0)

        def select(locator, label):
            locator.click()
            page.get_by_role('option', name=label, exact=True).click()

        def submit(dialog, button, path, status=201):
            with page.expect_response(lambda r: r.request.method == 'POST' and urlsplit(r.url).path == path) as received:
                dialog.get_by_role('button', name=button, exact=True).click()
            response = received.value
            check('ui_' + path + '_' + str(len(checks)), response.status == status, status=response.status)
            result = response.json().get('data')
            if status < 300:
                expect(dialog).not_to_be_visible()
            return result

        def metadata(page_name, code, name, extra=None):
            nav(page_name)
            page.get_by_role('button', name='Create', exact=True).click()
            dialog = page.get_by_role('dialog')
            dialog.get_by_label('Code', exact=True).fill(code)
            dialog.get_by_label('Name', exact=True).fill(name)
            if extra:
                extra(dialog)
            return dialog

        def widths(name, dialog=False):
            values = page.evaluate('''() => ({viewport:innerWidth,document:document.documentElement.scrollWidth,
              main:document.querySelector('main')?.clientWidth,mainScroll:document.querySelector('main')?.scrollWidth})''')
            if dialog:
                values.update(page.get_by_role('dialog').evaluate('(e)=>({dialog:e.clientWidth,dialogScroll:e.scrollWidth})'))
            check(name, values['document'] <= values['viewport'] and values['mainScroll'] <= values['main'] and
                  (not dialog or values['dialogScroll'] <= values['dialog']), **values)

        try:
            page.goto(base + '/login')
            page.get_by_label('Account', exact=True).fill('reader')
            page.get_by_label('Password', exact=True).fill(env['ADMIN_PASSWORD_READER'])
            page.get_by_role('button', name='Sign in', exact=True).click()
            expect(page.get_by_role('heading', name='Dashboard', exact=True)).to_be_visible()
            dialog=metadata('Currencies',prefix+'_DENIED','Read-only role canary',
                            lambda d:d.get_by_label('Decimal places (0-18)',exact=True).fill('2'))
            submit(dialog,'Create','/api/v1/currencies',403)
            expect(page.get_by_text(re.compile("You don't have permission")).first).to_be_visible()
            expect(dialog).to_be_visible()
            check('reader_denied_without_storage',not any(c['code']==prefix+'_DENIED' for c in rows('currencies?active_only=false')))
            save('reader-create-forbidden')
            page.keyboard.press('Escape')
            expect(dialog).not_to_be_visible()
            page.get_by_role('button',name='Sign out',exact=True).click()
            page.wait_for_url(base+'/login')
            check('logout_clears_session_cookie',not any(c['name']=='ledger_acceptance_session' for c in context.cookies()))
            page.goto(base + '/login')
            page.get_by_label('Account', exact=True).fill('admin')
            page.get_by_label('Password', exact=True).fill(env['ADMIN_PASSWORD_ADMIN'])
            page.get_by_role('button', name='Sign in', exact=True).click()
            expect(page.get_by_role('heading', name='Dashboard', exact=True)).to_be_visible()
            check('real_admin_login', True)
            check('identity_switch_clears_old_dialog_and_error',page.get_by_role('dialog').count()==0 and page.get_by_text(re.compile("You don't have permission")).count()==0)
            # Read all wired pages and assess responsive containment. Wide data tables may scroll inside their wrapper.
            names = ['Dashboard','Journals','Balances','Reservations','Deposits','Classifications',
                     'Journal Types','Templates','Currencies','Reconciliation','Snapshots']
            for name in names:
                nav(name)
                page.set_viewport_size({'width':390,'height':844})
                widths('mobile_' + name)
                if name in ('Currencies','Deposits','Templates'):
                    save(name.lower().replace(' ','-') + '-mobile')
                page.set_viewport_size({'width':1440,'height':1000})
            nav('Balances')
            page.get_by_label('Account Holder ID', exact=True).fill('1001')
            for code, amount in expected_amounts.items():
                select(page.get_by_label('Currency', exact=True), code)
                page.get_by_role('button', name='Search', exact=True).click()
                table = page.get_by_role('table', name='Booked balances for holder 1001', exact=True)
                button = table.get_by_role('button', name='Exact amount: ' + amount + ' ' + code, exact=True)
                expect(button).to_be_visible()
                button.focus()
                button.press('Enter')
                expect(page.get_by_role('tooltip')).to_contain_text(amount)
                check('exact_balance_' + code, True, amount=amount)
                if code == 'ATOM':
                    save('exact-atom-desktop')
                button.press('Escape')
            page.get_by_label('Account Holder ID', exact=True).fill('1002')
            page.get_by_role('button', name='Search', exact=True).click()
            tiny = page.get_by_role('table', name='Booked balances for holder 1002', exact=True).get_by_role('button', name='Exact amount: 0.000000000000000001 ATOM', exact=True)
            expect(tiny).to_be_visible()
            page.set_viewport_size({'width':390,'height':844})
            tiny.click()
            expect(page.get_by_role('tooltip')).to_contain_text('0.000000000000000001')
            widths('tiny_amount_mobile')
            save('tiny-atom-mobile')
            page.set_viewport_size({'width':1440,'height':1000})
            mixed = next(j for j in rows('journals?limit=100') if j['idempotency_key'] == 'admin-acceptance-v1:multi-currency')
            nav('Journals')
            page.locator('a[href="/journals/' + mixed['uid'] + '"]').click()
            totals = page.get_by_role('table', name='Journal totals by currency', exact=True)
            expect(totals).to_be_visible()
            check('mixed_journal_separate_currencies', totals.get_by_role('row').count() == 3 and
                  totals.get_by_role('button', name='Exact amount: 1.23 USD', exact=True).count() == 2 and
                  totals.get_by_role('button', name='Exact amount: 0.00000002 BTC', exact=True).count() == 2)
            save('mixed-journal-desktop')
            check('no_fund_flow_or_fake_entry_id', 'Fund Flow' not in page.locator('main').inner_text() and
                  'ID' not in page.get_by_role('table', name='Journal entries', exact=True).get_by_role('columnheader').all_text_contents())
            # Create and deactivate an integer currency, and verify duplicate errors stay visible.
            code = prefix + '_POINT'
            dialog = metadata('Currencies',code,'UI integer points',lambda d:d.get_by_label('Decimal places (0-18)',exact=True).fill('0'))
            dialog.get_by_label('Decimal places (0-18)',exact=True).fill('2.5')
            dialog.get_by_role('button',name='Create',exact=True).click()
            expect(dialog.get_by_text('Enter a whole number from 0 to 18.',exact=True)).to_be_visible()
            check('fractional_exponent_rejected_without_storage',not any(c['code']==code for c in rows('currencies?active_only=false')))
            dialog.get_by_label('Decimal places (0-18)',exact=True).fill('0')
            created = submit(dialog,'Create','/api/v1/currencies')
            check('currency_exponent_zero_persisted', next(c for c in rows('currencies') if c['code']==code)['exponent']==0)
            dialog = metadata('Currencies',code,'duplicate',lambda d:d.get_by_label('Decimal places (0-18)',exact=True).fill('0'))
            submit(dialog,'Create','/api/v1/currencies',409)
            expect(dialog).to_be_visible()
            page.keyboard.press('Escape')
            expect(dialog).not_to_be_visible()
            row = page.get_by_role('row').filter(has_text=code)
            row.get_by_role('button',name='Deactivate',exact=True).click()
            submit(page.get_by_role('dialog'),'Deactivate','/api/v1/currencies/'+created['uid']+'/deactivate',200)
            check('currency_deactivated', not next(c for c in rows('currencies?active_only=false') if c['code']==code)['is_active'])
            classifications=[]
            for suffix, owner in [('D','Account holder'),('C','Account holder'),('SYS','System')]:
                def configure(d, owner=owner):
                    select(d.get_by_label('Account owner',exact=True),owner)
                    select(d.get_by_label('Balance Role',exact=True),'Memo — cost / memo account, not a liability')
                class_code=prefix+'_'+suffix
                dialog=metadata('Classifications',class_code,class_code,configure)
                classification=submit(dialog,'Create','/api/v1/classifications')
                classifications.append(classification)
                check('classification_owner_'+suffix, classification['is_system']==(owner=='System'))
            jt_code=prefix+'_JT'
            dialog=metadata('Journal Types',jt_code,jt_code)
            journal_type=submit(dialog,'Create','/api/v1/journal-types')
            nav('Templates')
            page.set_viewport_size({'width':390,'height':844})
            page.get_by_role('button',name='Create Template',exact=True).click()
            dialog=page.get_by_role('dialog')
            dialog.get_by_label('Code',exact=True).fill(prefix+'_TPL')
            dialog.get_by_label('Name',exact=True).fill(prefix+'_TPL')
            select(dialog.get_by_label('Journal Type',exact=True),jt_code)
            expect(dialog.get_by_label('Journal Type',exact=True)).to_contain_text(jt_code)
            check('selected_journal_type_label_'+str(len(checks)),True)
            select(dialog.get_by_label('Debit classification 1',exact=True),prefix+'_D')
            select(dialog.get_by_label('Credit classification 2',exact=True),prefix+'_C')
            select(dialog.get_by_label('Credit holder 2',exact=True),'User')
            widths('template_create_mobile',dialog=True)
            for label in ['Debit amount key 1','Credit amount key 2']:
                box=dialog.get_by_label(label,exact=True).bounding_box()
                check('usable_'+label, box is not None and box['width']>=100 and box['x']>=0 and box['x']+box['width']<=390)
            save('template-create-mobile')
            template=submit(dialog,'Create','/api/v1/templates')
            page.set_viewport_size({'width':1440,'height':1000})
            # Preview through the template card, then reject an over-precision journal without persisting it.
            card=page.locator('[data-slot="card"]').filter(has=page.get_by_text(prefix+'_TPL',exact=True)).first
            card.get_by_role('button',name='Preview',exact=True).click()
            card.get_by_label('Holder ID',exact=True).fill('990101')
            select(card.get_by_role('combobox'),'USD')
            expect(card.get_by_role('combobox')).to_contain_text('USD')
            check('preview_currency_label',True)
            card.get_by_label('Amount',exact=True).fill('1.23')
            with page.expect_response(lambda r:'/templates/'+prefix+'_TPL/preview' in r.url) as preview_response:
                card.get_by_role('button',name='Preview',exact=True).last.click()
            check('template_preview_without_post',preview_response.value.status==200 and
                  len(preview_response.value.json()['data']['entries'])==2)
            nav('Journals')
            page.get_by_role('button',name='Post Journal',exact=True).click()
            dialog=page.get_by_role('dialog')
            select(dialog.get_by_label('Journal Type',exact=True),jt_code)
            expect(dialog.get_by_label('Journal Type',exact=True)).to_contain_text(jt_code)
            check('selected_journal_type_label_'+str(len(checks)),True)
            dialog.get_by_label('Idempotency Key',exact=True).fill(prefix+':invalid-precision')
            entries=[{'account_holder':990102,'currency_uid':usd,'classification_uid':classifications[i]['uid'],
                      'entry_type':side,'amount':'0.001'} for i,side in enumerate(['debit','credit'])]
            dialog.get_by_label('Entries (JSON array)',exact=True).fill(json.dumps(entries))
            with page.expect_response(lambda r:r.request.method=='POST' and urlsplit(r.url).path=='/api/v1/journals') as rejected:
                dialog.get_by_role('button',name='Post',exact=True).click()
            check('over_precision_rejected_atomically',rejected.value.status==422 and rejected.value.json()['code']==14006 and
                  not any(j['idempotency_key']==prefix+':invalid-precision' for j in rows('journals?limit=100')))
            expect(dialog).to_be_visible()
            page.keyboard.press('Escape')
            expect(dialog).not_to_be_visible()
            # Posting from the public template UI uses decimal strings and non-system classifications.
            nav('Journals')
            page.get_by_role('button',name='Template Journal',exact=True).click()
            dialog=page.get_by_role('dialog')
            select(dialog.get_by_label('Template',exact=True),prefix+'_TPL')
            expect(dialog.get_by_label('Template',exact=True)).to_contain_text(prefix+'_TPL')
            check('selected_template_label',True)
            dialog.get_by_label('Holder ID',exact=True).fill('990101')
            select(dialog.get_by_label('Currency',exact=True),'USD')
            expect(dialog.get_by_label('Currency',exact=True)).to_contain_text('USD')
            check('selected_template_currency_label',True)
            dialog.get_by_label('Idempotency Key',exact=True).fill(prefix+':template')
            dialog.get_by_label('Amounts (JSON object)',exact=True).fill('{"amount":"1.23"}')
            posted=submit(dialog,'Post','/api/v1/journals/template')
            uid=posted['uid']
            wire=http('GET','journals/'+uid)
            check('template_post_decimal_balanced',len(wire['entries'])==2 and all(Decimal(e['amount'])==Decimal('1.23') for e in wire['entries']))
            page.locator('a[href="/journals/'+uid+'"]').click()
            expect(page.get_by_role('table',name='Journal entries',exact=True)).to_be_visible()
            page.get_by_role('button',name='Reverse',exact=True).click()
            dialog=page.get_by_role('dialog')
            dialog.get_by_label('Reason',exact=True).fill('Isolated acceptance reversal')
            submit(dialog,'Confirm Reverse','/api/v1/journals/'+uid+'/reverse',201)
            balances=rows('balances/990101?currency_uid='+usd)
            check('reversal_restores_each_classification',all(Decimal(r['balance'])==0 for r in balances))
            # Holds use the fixture account but never touch its existing hold.
            def reserve(amount,suffix):
                return http('POST','reservations',{'account_holder':1001,'currency_uid':usd,'amount':amount,
                            'idempotency_key':prefix+':'+suffix,'expires_in_sec':3600},201)
            hold=reserve('20','partial')
            check('hold_reduces_only_available',Decimal(breakdown()['available'])==Decimal('1180') and Decimal(breakdown()['total'])==Decimal('1234.56'))
            journal_ids={j['uid'] for j in rows('journals?limit=100')}
            nav('Reservations')
            row=page.get_by_role('row').filter(has_text=hold['uid'])
            row.get_by_role('button',name='Settle Partial',exact=True).click()
            dialog=page.get_by_role('dialog')
            expect(dialog).to_contain_text('Booked balances do not change')
            dialog.get_by_label('Partial Amount',exact=True).fill('5')
            submit(dialog,'Settle Partial','/api/v1/reservations/'+hold['uid']+'/settle-partial',200)
            check('partial_releases_five_without_debit',Decimal(breakdown()['available'])==Decimal('1185') and Decimal(breakdown()['total'])==Decimal('1234.56'))
            row.get_by_role('button',name='Finalize',exact=True).click()
            submit(page.get_by_role('dialog'),'Finalize','/api/v1/reservations/'+hold['uid']+'/finalize',200)
            check('finalize_releases_remainder',Decimal(breakdown()['available'])==Decimal('1200'))
            hold=reserve('30','settle')
            nav('Dashboard'); nav('Reservations')
            page.get_by_role('row').filter(has_text=hold['uid']).get_by_role('button',name='Settle',exact=True).click()
            dialog=page.get_by_role('dialog')
            expect(dialog).to_contain_text('does not post a charge')
            dialog.get_by_label('Actual Amount',exact=True).fill('25')
            submit(dialog,'Settle','/api/v1/reservations/'+hold['uid']+'/settle',200)
            check('settle_no_booked_debit_or_new_journal',Decimal(breakdown()['available'])==Decimal('1200') and
                  Decimal(breakdown()['total'])==Decimal('1234.56') and journal_ids=={j['uid'] for j in rows('journals?limit=100')})
            nav('Reconciliation')
            with page.expect_response(lambda r:urlsplit(r.url).path=='/api/v1/reconcile') as response:
                page.get_by_role('button',name='Run Global Check',exact=True).click()
            check('global_reconciliation',response.value.status==200 and response.value.json()['data']['balanced'])
            page.get_by_label('Holder',exact=True).fill('1001')
            select(page.get_by_label('Currency',exact=True),'USD')
            with page.expect_response(lambda r:urlsplit(r.url).path=='/api/v1/reconcile/account') as response:
                page.get_by_role('button',name='Check',exact=True).click()
            check('account_reconciliation',response.value.status==200 and response.value.json()['data']['balanced'])
            save('reconciliation-desktop')
            check('no_uncaught_browser_errors',not errors,errors=errors)
        except BaseException as error:
            save('failure')
            checks.append({'case':'execution','pass':False,'error':str(error)})
            raise
        finally:
            result={'prefix':prefix,'checks':checks,'passed':sum(c['pass'] for c in checks),
                    'failed':sum(not c['pass'] for c in checks),'page_errors':errors}
            (args.output/'results.json').write_text(json.dumps(result,indent=2)+'\n')
            context.close(); browser.close()


if __name__ == '__main__':
    main()
