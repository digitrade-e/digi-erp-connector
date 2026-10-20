<?php
// Produces wire.json and wire-blanks.json: the backend's REAL Receipt (DIG-928, client-instance/backend
// api/src/Model/Outbound/Receipt.php) serialized exactly as the outbound HTTP path sends it —
// GenericDocumentSender passes jsonSerialize(), and Symfony HttpClient's 'json' option encodes the body
// (HttpClientTrait::jsonEncode: JSON_HEX_TAG|APOS|AMP|QUOT, so Hebrew and quotes arrive as \uXXXX).
//
// Regenerate (read-only, no network, nothing touches a database):
//   mkdir -p /tmp/w/in /tmp/w/out && cp generate.php /tmp/w/in/gen.php
//   git -C <backend clone> show origin/feat/DIG-928-send-receipt:api/src/Model/Outbound/Receipt.php > /tmp/w/in/Receipt.php
//   docker run --rm --network none -v /tmp/w/in:/in:ro -v /tmp/w/out:/out --entrypoint php backend-php:latest /in/gen.php
// then copy /tmp/w/out/*.json here and update the hashes in send_receipt_test.go.
require '/srv/app/vendor/autoload.php';
require '/in/Receipt.php';
use App\Model\Outbound\Receipt;
use Symfony\Component\HttpClient\MockHttpClient;
use Symfony\Component\HttpClient\Response\MockResponse;

function body(Receipt $r): string {
    $captured = null;
    $client = new MockHttpClient(function ($method, $url, $options) use (&$captured) {
        $captured = $options['body'];
        return new MockResponse('{}');
    });
    $client->request('POST', 'https://connector.example.invalid/api/sendReceipt', ['json' => $r->jsonSerialize()])->getContent();
    return $captured;
}
// ReceiptRequestBodyTest::receipt() values, verbatim.
$full = new Receipt(
    idempotencyKey: '7c9e6679-7425-40de-944b-e07fc1f90ae7', accountKey: '30021', settledDate: '2026-10-14',
    amountMinor: 693200, installments: 1, cardIssuer: '2', cardBrand: '2', last4: '4242', confirmationCode: '0505293',
    providerUid: '25101412345678901234567', remarks: '26004295, 19057', agentKey: '7', name: 'בל"פ', address: 'הרצל 1',
    phone: '03-5555555', email: 'pay@example.test',
);
// Everything ReceiptBuilder may leave blank, blank; an odd amount; a cut remark list.
$blanks = new Receipt(
    idempotencyKey: '0b8f6a4e-3c1d-4e2f-9a7b-5c6d7e8f9a0b', accountKey: '100234', settledDate: '2026-12-31',
    amountMinor: 1, installments: 1, cardIssuer: '', cardBrand: '', last4: '0001', confirmationCode: '1',
    providerUid: '', remarks: "INV-1 +2", agentKey: '', name: "צ'יפס & <בע\"מ>", address: '',
    phone: '', email: '',
);
file_put_contents('/out/wire.json', body($full));
file_put_contents('/out/wire-blanks.json', body($blanks));
