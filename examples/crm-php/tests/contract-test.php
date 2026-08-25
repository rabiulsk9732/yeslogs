<?php

declare(strict_types=1);

define('YESLOGS_CRM_LIBRARY_ONLY', true);
require dirname(__DIR__) . '/public/session-lookups.php';

function expect(bool $condition, string $message): void
{
    if (!$condition) {
        throw new RuntimeException($message);
    }
}

$requestId = 'YLREQ-3C15D73E92F2A80F8C7B4D11';
$reference1 = 'YL-20260717-2A3C5E7910BF4D827639A1C0';
$reference2 = 'YL-20260717-111111111111111111111111';
$reference3 = 'YL-20260717-222222222222222222222222';

$lookup1 = validateLookup([
    'referenceCode' => $reference1,
    'localIp' => '172.16.18.146',
    'eventTime' => '2026-07-17T08:16:19Z',
    'deviceId' => 4,
    'nasIdentifier' => 'wadhai-mikrotik-nas',
    'nasIpAddress' => '198.51.100.10',
], 0, $requestId, new DateTimeZone('Asia/Kolkata'));

expect($lookup1['event_time'] === '2026-07-17 13:46:19', 'UTC-to-radacct timezone conversion failed');

$lookup2 = $lookup1;
$lookup2['seq'] = 1;
$lookup2['reference_code'] = $reference2;
$lookup3 = $lookup1;
$lookup3['seq'] = 2;
$lookup3['reference_code'] = $reference3;

$session = [
    'radacctid' => '1001',
    'acctsessionid' => 'RAD-SESSION-123',
    'username' => 'radius-user-31',
    'acctstarttime' => '2026-07-17 12:00:00',
    'account_id' => 'CUST-90031',
    'subscriber_name' => 'Customer Name',
    'subscriber_address' => 'Customer Address',
    'subscriber_phone' => '9999999999',
];

$matches = [
    $reference1 => ['1001' => $session],
    $reference3 => [
        '2001' => $session,
        '2002' => array_merge($session, ['radacctid' => '2002', 'acctsessionid' => 'RAD-SESSION-OTHER']),
    ],
];

$results = buildResults([$lookup1, $lookup2, $lookup3], $matches);
expect(count($results) === 3, 'Result count mismatch');
expect($results[0]['status'] === 'matched', 'Matched status missing');
expect($results[0]['referenceCode'] === $reference1, 'referenceCode was not preserved');
expect($results[0]['subscriber']['username'] === 'radius-user-31', 'Subscriber username missing');
expect($results[0]['subscriber']['phone'] === '9999999999', 'Subscriber phone missing');
expect($results[1]['status'] === 'not_found', 'not_found status missing');
expect($results[2]['status'] === 'ambiguous', 'ambiguous status missing');

$unicode = safeText('नमस्ते', 4);
expect($unicode !== '' && preg_match('//u', $unicode) === 1, 'UTF-8 truncation is invalid');
expect(quoteQualifiedIdentifier('radius.radacct') === '`radius`.`radacct`', 'Identifier quoting failed');

$invalidRejected = false;
try {
    validateLookup([
        'referenceCode' => $reference1,
        'localIp' => 'not-an-ip',
        'eventTime' => '2026-07-17T08:16:19Z',
        'deviceId' => 4,
    ], 0, $requestId, new DateTimeZone('UTC'));
} catch (ApiError $error) {
    $invalidRejected = $error->errorCode === 'invalid_local_ip';
}
expect($invalidRejected, 'Invalid local IP was not rejected');

echo "YesLogs PHP contract tests passed\n";
