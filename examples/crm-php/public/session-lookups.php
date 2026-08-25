<?php

declare(strict_types=1);

/*
 * YesLogs CRM/RADIUS reference endpoint.
 *
 * Contract: yeslogs.crm.lookup.v1
 * Runtime:  PHP 8.1+ with PDO MySQL
 *
 * This implementation performs one bulk insert into a connection-local
 * temporary table and one indexed radacct join for the whole request. It does
 * not issue one SQL query per IPDR row.
 */

const YESLOGS_SCHEMA = 'yeslogs.crm.lookup.v1';
const YESLOGS_MAX_REFERENCE_LENGTH = 80;
const YESLOGS_MAX_REQUEST_ID_LENGTH = 80;

final class ApiError extends RuntimeException
{
    public function __construct(
        public readonly int $httpStatus,
        public readonly string $errorCode,
        string $message,
        public readonly ?string $requestId = null
    ) {
        parent::__construct($message);
    }
}

if (!defined('YESLOGS_CRM_LIBRARY_ONLY')) {
    runEndpoint();
}

function runEndpoint(): never
{
    try {
        $configPath = getenv('YESLOGS_CRM_CONFIG');
        if ($configPath === false || trim((string) $configPath) === '') {
            $configPath = dirname(__DIR__) . '/config.php';
        }
        if (!is_file((string) $configPath)) {
            throw new RuntimeException('CRM API configuration file is missing');
        }

        /** @var mixed $loadedConfig */
        $loadedConfig = require (string) $configPath;
        if (!is_array($loadedConfig)) {
            throw new RuntimeException('CRM API configuration must return an array');
        }

        handleRequest($loadedConfig);
    } catch (ApiError $error) {
        if ($error->httpStatus === 401) {
            header('WWW-Authenticate: Bearer realm="YesLogs CRM"');
        }
        if ($error->httpStatus === 405) {
            header('Allow: POST');
        }
        sendJson($error->httpStatus, [
            'schemaVersion' => YESLOGS_SCHEMA,
            'requestId' => $error->requestId,
            'error' => [
                'code' => $error->errorCode,
                'message' => $error->getMessage(),
            ],
        ]);
    } catch (Throwable $error) {
        // Never log API keys, request bodies, local IPs, usernames, or SQL details.
        error_log(sprintf(
            'YesLogs CRM endpoint internal failure (%s)',
            get_class($error)
        ));
        sendJson(500, [
            'schemaVersion' => YESLOGS_SCHEMA,
            'requestId' => null,
            'error' => [
                'code' => 'internal_error',
                'message' => 'CRM lookup is temporarily unavailable',
            ],
        ]);
    }
}

/**
 * @param array<string,mixed> $config
 */
function handleRequest(array $config): never
{
    header('Cache-Control: no-store, max-age=0');
    header('X-Content-Type-Options: nosniff');

    if (($_SERVER['REQUEST_METHOD'] ?? '') !== 'POST') {
        throw new ApiError(405, 'method_not_allowed', 'Use POST');
    }

    $contentType = strtolower(trim((string) ($_SERVER['CONTENT_TYPE'] ?? '')));
    if (!str_starts_with($contentType, 'application/json')) {
        throw new ApiError(415, 'unsupported_media_type', 'Content-Type must be application/json');
    }

    authenticateRequest($config);

    $schemaHeader = requestHeader('X-YesLogs-Schema');
    if (!hash_equals(YESLOGS_SCHEMA, $schemaHeader)) {
        throw new ApiError(400, 'schema_mismatch', 'Unsupported YesLogs schema');
    }

    $maxBodyBytes = positiveConfigInt($config, 'max_body_bytes', 2 * 1024 * 1024, 16 * 1024 * 1024);
    $contentLength = (int) ($_SERVER['CONTENT_LENGTH'] ?? 0);
    if ($contentLength > $maxBodyBytes) {
        throw new ApiError(413, 'body_too_large', 'Request body is too large');
    }

    $rawBody = file_get_contents('php://input', false, null, 0, $maxBodyBytes + 1);
    if ($rawBody === false) {
        throw new ApiError(400, 'invalid_body', 'Could not read request body');
    }
    if (strlen($rawBody) > $maxBodyBytes) {
        throw new ApiError(413, 'body_too_large', 'Request body is too large');
    }

    try {
        /** @var mixed $decoded */
        $decoded = json_decode($rawBody, true, 32, JSON_THROW_ON_ERROR);
    } catch (JsonException) {
        throw new ApiError(400, 'invalid_json', 'Request body is not valid JSON');
    }
    if (!is_array($decoded) || array_is_list($decoded)) {
        throw new ApiError(400, 'invalid_request', 'JSON root must be an object');
    }

    $requestId = validateRequestId($decoded['requestId'] ?? null);
    if (($decoded['schemaVersion'] ?? null) !== YESLOGS_SCHEMA) {
        throw new ApiError(400, 'schema_mismatch', 'Unsupported YesLogs schema', $requestId);
    }

    $idempotencyKey = requestHeader('Idempotency-Key');
    if ($idempotencyKey !== '' && !hash_equals($requestId, $idempotencyKey)) {
        throw new ApiError(400, 'idempotency_mismatch', 'Idempotency-Key must equal requestId', $requestId);
    }

    $lookupsValue = $decoded['lookups'] ?? null;
    if (!is_array($lookupsValue) || !array_is_list($lookupsValue)) {
        throw new ApiError(400, 'invalid_lookups', 'lookups must be a JSON array', $requestId);
    }
    $maxLookups = positiveConfigInt($config, 'max_lookups', 1000, 1000);
    if (count($lookupsValue) < 1 || count($lookupsValue) > $maxLookups) {
        throw new ApiError(400, 'invalid_lookup_count', "lookups must contain 1-{$maxLookups} items", $requestId);
    }

    $databaseConfig = $config['database'] ?? null;
    if (!is_array($databaseConfig)) {
        throw new RuntimeException('database configuration is missing');
    }
    $radacctTimezone = configuredTimezone($databaseConfig['radacct_timezone'] ?? 'UTC');

    /** @var list<array<string,mixed>> $lookups */
    $lookups = [];
    /** @var array<string,true> $seenReferences */
    $seenReferences = [];
    foreach ($lookupsValue as $index => $lookupValue) {
        if (!is_array($lookupValue) || array_is_list($lookupValue)) {
            throw new ApiError(400, 'invalid_lookup', "lookups[{$index}] must be an object", $requestId);
        }
        $lookup = validateLookup($lookupValue, (int) $index, $requestId, $radacctTimezone);
        $reference = (string) $lookup['reference_code'];
        if (isset($seenReferences[$reference])) {
            throw new ApiError(400, 'duplicate_reference', "Duplicate referenceCode at lookups[{$index}]", $requestId);
        }
        $seenReferences[$reference] = true;
        $lookups[] = $lookup;
    }

    $pdo = connectDatabase($databaseConfig);
    $matches = findMatches($pdo, $config, $lookups);
    $results = buildResults($lookups, $matches);

    sendJson(200, [
        'schemaVersion' => YESLOGS_SCHEMA,
        'requestId' => $requestId,
        'results' => $results,
    ]);
}

/**
 * @param array<string,mixed> $config
 */
function authenticateRequest(array $config): void
{
    $expected = trim((string) ($config['api_key'] ?? ''));
    if ($expected === '') {
        throw new RuntimeException('YESLOGS_API_KEY is not configured');
    }

    $authorization = requestHeader('Authorization');
    if (!preg_match('/^Bearer[ \t]+(.+)$/i', $authorization, $match)) {
        throw new ApiError(401, 'unauthorized', 'Valid Bearer authentication is required');
    }
    $provided = trim((string) $match[1]);
    if ($provided === '' || !hash_equals($expected, $provided)) {
        throw new ApiError(401, 'unauthorized', 'Valid Bearer authentication is required');
    }
}

function requestHeader(string $name): string
{
    $serverKey = 'HTTP_' . strtoupper(str_replace('-', '_', $name));
    if (isset($_SERVER[$serverKey])) {
        return trim((string) $_SERVER[$serverKey]);
    }

    // Some Apache/FastCGI setups preserve Authorization under this key.
    if (strcasecmp($name, 'Authorization') === 0 && isset($_SERVER['REDIRECT_HTTP_AUTHORIZATION'])) {
        return trim((string) $_SERVER['REDIRECT_HTTP_AUTHORIZATION']);
    }

    if (function_exists('getallheaders')) {
        foreach ((array) getallheaders() as $headerName => $value) {
            if (strcasecmp((string) $headerName, $name) === 0) {
                return trim((string) $value);
            }
        }
    }
    return '';
}

function validateRequestId(mixed $value): string
{
    if (!is_string($value)
        || strlen($value) > YESLOGS_MAX_REQUEST_ID_LENGTH
        || !preg_match('/^YLREQ-[A-F0-9]{24}$/', $value)
    ) {
        throw new ApiError(400, 'invalid_request_id', 'requestId is invalid');
    }
    return $value;
}

/**
 * @param array<string,mixed> $value
 * @return array<string,mixed>
 */
function validateLookup(
    array $value,
    int $index,
    string $requestId,
    DateTimeZone $radacctTimezone
): array {
    $reference = $value['referenceCode'] ?? null;
    if (!is_string($reference)
        || strlen($reference) > YESLOGS_MAX_REFERENCE_LENGTH
        || !preg_match('/^YL-[0-9]{8}-[A-F0-9]{24}$/', $reference)
    ) {
        throw new ApiError(400, 'invalid_reference', "Invalid referenceCode at lookups[{$index}]", $requestId);
    }

    $localIp = $value['localIp'] ?? null;
    if (!is_string($localIp) || filter_var($localIp, FILTER_VALIDATE_IP) === false) {
        throw new ApiError(400, 'invalid_local_ip', "Invalid localIp at lookups[{$index}]", $requestId);
    }

    $eventTimeValue = $value['eventTime'] ?? null;
    if (!is_string($eventTimeValue)
        || !preg_match('/^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]{1,9})?(?:Z|[+-][0-9]{2}:[0-9]{2})$/', $eventTimeValue)
    ) {
        throw new ApiError(400, 'invalid_event_time', "Invalid eventTime at lookups[{$index}]", $requestId);
    }
    try {
        $eventTime = new DateTimeImmutable($eventTimeValue);
    } catch (Exception) {
        throw new ApiError(400, 'invalid_event_time', "Invalid eventTime at lookups[{$index}]", $requestId);
    }

    $deviceId = $value['deviceId'] ?? null;
    if (!is_int($deviceId) || $deviceId < 1 || $deviceId > 4294967295) {
        throw new ApiError(400, 'invalid_device_id', "Invalid deviceId at lookups[{$index}]", $requestId);
    }

    $nasIdentifier = optionalString($value, 'nasIdentifier', 190, $index, $requestId);
    $nasIpAddress = optionalString($value, 'nasIpAddress', 45, $index, $requestId);
    if ($nasIpAddress !== '' && filter_var($nasIpAddress, FILTER_VALIDATE_IP) === false) {
        throw new ApiError(400, 'invalid_nas_ip', "Invalid nasIpAddress at lookups[{$index}]", $requestId);
    }

    return [
        'seq' => $index,
        'reference_code' => $reference,
        'local_ip' => $localIp,
        // radacct normally uses DATETIME; convert the unambiguous RFC3339 input
        // into the exact timezone used by that CRM's radacct table.
        'event_time' => $eventTime->setTimezone($radacctTimezone)->format('Y-m-d H:i:s'),
        'device_id' => $deviceId,
        'nas_identifier' => $nasIdentifier,
        'nas_ip_address' => $nasIpAddress,
    ];
}

/**
 * @param array<string,mixed> $value
 */
function optionalString(
    array $value,
    string $field,
    int $maxLength,
    int $index,
    string $requestId
): string {
    if (!array_key_exists($field, $value) || $value[$field] === null) {
        return '';
    }
    if (!is_string($value[$field]) || strlen($value[$field]) > $maxLength) {
        throw new ApiError(400, 'invalid_lookup', "Invalid {$field} at lookups[{$index}]", $requestId);
    }
    return trim($value[$field]);
}

/**
 * @param array<string,mixed> $databaseConfig
 */
function connectDatabase(array $databaseConfig): PDO
{
    if (!extension_loaded('pdo_mysql')) {
        throw new RuntimeException('PDO MySQL extension is not installed');
    }

    $dsn = trim((string) ($databaseConfig['dsn'] ?? ''));
    if ($dsn === '') {
        throw new RuntimeException('CRM_DB_DSN is not configured');
    }
    return new PDO(
        $dsn,
        (string) ($databaseConfig['user'] ?? ''),
        (string) ($databaseConfig['password'] ?? ''),
        [
            PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION,
            PDO::ATTR_DEFAULT_FETCH_MODE => PDO::FETCH_ASSOC,
            PDO::ATTR_EMULATE_PREPARES => false,
            PDO::ATTR_PERSISTENT => false,
            PDO::ATTR_TIMEOUT => 5,
        ]
    );
}

/**
 * @param array<string,mixed> $config
 * @param list<array<string,mixed>> $lookups
 * @return array<string,array<string,array<string,mixed>>>
 */
function findMatches(PDO $pdo, array $config, array $lookups): array
{
    $radacctTable = quoteQualifiedIdentifier((string) ($config['radacct_table'] ?? 'radacct'));
    $subscriberView = quoteQualifiedIdentifier((string) ($config['subscriber_view'] ?? 'yeslogs_subscribers'));
    $nasMatchMode = strtolower(trim((string) ($config['nas_match_mode'] ?? 'when_present')));
    $nasPredicate = match ($nasMatchMode) {
        'required' => 'AND r.`nasipaddress` = l.`nas_ip_address`',
        'ignore' => '',
        'when_present' => "AND (l.`nas_ip_address` = '' OR r.`nasipaddress` = l.`nas_ip_address`)",
        default => throw new RuntimeException('CRM_NAS_MATCH_MODE must be required, when_present, or ignore'),
    };

    $pdo->exec('DROP TEMPORARY TABLE IF EXISTS `yeslogs_lookup_batch`');
    $pdo->exec(
        'CREATE TEMPORARY TABLE `yeslogs_lookup_batch` (
            `seq` SMALLINT UNSIGNED NOT NULL,
            `reference_code` VARCHAR(80) NOT NULL,
            `local_ip` VARCHAR(45) NOT NULL,
            `event_time` DATETIME NOT NULL,
            `device_id` INT UNSIGNED NOT NULL,
            `nas_identifier` VARCHAR(190) NOT NULL,
            `nas_ip_address` VARCHAR(45) NOT NULL,
            PRIMARY KEY (`reference_code`),
            KEY `idx_local_time` (`local_ip`, `event_time`)
        ) ENGINE=MEMORY'
    );

    foreach (array_chunk($lookups, 200) as $chunk) {
        $placeholders = [];
        $params = [];
        foreach ($chunk as $lookup) {
            $placeholders[] = '(?,?,?,?,?,?,?)';
            array_push(
                $params,
                $lookup['seq'],
                $lookup['reference_code'],
                $lookup['local_ip'],
                $lookup['event_time'],
                $lookup['device_id'],
                $lookup['nas_identifier'],
                $lookup['nas_ip_address']
            );
        }
        $insert = $pdo->prepare(
            'INSERT INTO `yeslogs_lookup_batch`
                (`seq`,`reference_code`,`local_ip`,`event_time`,`device_id`,`nas_identifier`,`nas_ip_address`)
             VALUES ' . implode(',', $placeholders)
        );
        $insert->execute($params);
    }

    /*
     * Vendor adaptation point:
     * - radacct columns below are standard FreeRADIUS names.
     * - yeslogs_subscribers is a view with normalized fields:
     *   username, account_id, name, address, phone.
     * - If the CRM identifies a NAS differently, replace $nasPredicate or join
     *   a device/NAS mapping table here.
     */
    $sql = "
        SELECT
            l.`reference_code`,
            r.`radacctid`,
            r.`acctsessionid`,
            r.`username`,
            r.`acctstarttime`,
            COALESCE(CAST(s.`account_id` AS CHAR), '') AS `account_id`,
            COALESCE(s.`name`, '') AS `subscriber_name`,
            COALESCE(s.`address`, '') AS `subscriber_address`,
            COALESCE(CAST(s.`phone` AS CHAR), '') AS `subscriber_phone`
        FROM `yeslogs_lookup_batch` l
        INNER JOIN {$radacctTable} r
            ON r.`framedipaddress` = l.`local_ip`
           AND r.`acctstarttime` <= l.`event_time`
           AND (r.`acctstoptime` IS NULL OR r.`acctstoptime` >= l.`event_time`)
           {$nasPredicate}
        LEFT JOIN {$subscriberView} s
            ON s.`username` = r.`username`
        ORDER BY l.`seq`, r.`acctstarttime` DESC, r.`radacctid` DESC
    ";

    $statement = $pdo->query($sql);
    /** @var array<string,array<string,array<string,mixed>>> $matches */
    $matches = [];
    while (($row = $statement->fetch()) !== false) {
        $reference = (string) $row['reference_code'];
        $sessionKey = (string) $row['radacctid'];
        if ($sessionKey === '') {
            $sessionKey = implode('|', [
                (string) $row['acctsessionid'],
                (string) $row['username'],
                (string) $row['acctstarttime'],
            ]);
        }
        // Key by session so an accidental duplicate subscriber join cannot turn
        // one RADIUS session into a false "ambiguous" result.
        $matches[$reference][$sessionKey] = $row;
    }
    return $matches;
}

/**
 * @param list<array<string,mixed>> $lookups
 * @param array<string,array<string,array<string,mixed>>> $matches
 * @return list<array<string,mixed>>
 */
function buildResults(array $lookups, array $matches): array
{
    $results = [];
    foreach ($lookups as $lookup) {
        $reference = (string) $lookup['reference_code'];
        $sessions = array_values($matches[$reference] ?? []);
        if (count($sessions) === 0) {
            $results[] = emptyResult($reference, 'not_found');
            continue;
        }
        if (count($sessions) > 1) {
            // Never guess when stale/open or overlapping sessions exist.
            $results[] = emptyResult($reference, 'ambiguous');
            continue;
        }

        $session = $sessions[0];
        $results[] = [
            'referenceCode' => $reference,
            'status' => 'matched',
            'subscriber' => [
                'accountId' => safeText($session['account_id'] ?? '', 190),
                'username' => safeText($session['username'] ?? '', 190),
                'name' => safeText($session['subscriber_name'] ?? '', 256),
                'address' => safeText($session['subscriber_address'] ?? '', 1024),
                'phone' => safeText($session['subscriber_phone'] ?? '', 64),
            ],
            'session' => [
                'acctSessionId' => safeText($session['acctsessionid'] ?? '', 190),
            ],
        ];
    }
    return $results;
}

/**
 * @return array<string,mixed>
 */
function emptyResult(string $reference, string $status): array
{
    return [
        'referenceCode' => $reference,
        'status' => $status,
        'subscriber' => [
            'accountId' => '',
            'username' => '',
            'name' => '',
            'address' => '',
            'phone' => '',
        ],
        'session' => [
            'acctSessionId' => '',
        ],
    ];
}

function safeText(mixed $value, int $maxBytes): string
{
    $text = trim((string) $value);
    if (strlen($text) <= $maxBytes) {
        return $text;
    }
    $text = substr($text, 0, $maxBytes);
    // Back up to a valid UTF-8 boundary without requiring mbstring.
    while ($text !== '' && preg_match('//u', $text) !== 1) {
        $text = substr($text, 0, -1);
    }
    return $text;
}

function quoteQualifiedIdentifier(string $identifier): string
{
    $parts = explode('.', trim($identifier));
    if ($parts === []) {
        throw new RuntimeException('Database identifier is empty');
    }
    $quoted = [];
    foreach ($parts as $part) {
        if (!preg_match('/^[A-Za-z_][A-Za-z0-9_]*$/', $part)) {
            throw new RuntimeException('Database identifier is invalid');
        }
        $quoted[] = '`' . $part . '`';
    }
    return implode('.', $quoted);
}

function configuredTimezone(mixed $value): DateTimeZone
{
    try {
        return new DateTimeZone(trim((string) $value));
    } catch (Exception) {
        throw new RuntimeException('CRM_RADACCT_TIMEZONE is invalid');
    }
}

/**
 * @param array<string,mixed> $config
 */
function positiveConfigInt(array $config, string $key, int $default, int $maximum): int
{
    $value = $config[$key] ?? $default;
    if (!is_int($value) || $value < 1 || $value > $maximum) {
        throw new RuntimeException("Invalid {$key} configuration");
    }
    return $value;
}

/**
 * @param array<string,mixed> $payload
 */
function sendJson(int $status, array $payload): never
{
    http_response_code($status);
    header('Content-Type: application/json; charset=utf-8');
    header('Cache-Control: no-store, max-age=0');
    header('X-Content-Type-Options: nosniff');
    echo json_encode(
        $payload,
        JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE | JSON_THROW_ON_ERROR
    );
    exit;
}
