<?php

declare(strict_types=1);

/*
 * Copy this file to ../config.php (outside the public web root), or set
 * YESLOGS_CRM_CONFIG to another absolute path.
 *
 * In production, inject secrets through PHP-FPM/systemd/container environment
 * variables or a secret manager. Do not commit the real API key/password.
 */

$env = static function (string $name, string $default = ''): string {
    $value = getenv($name);
    return $value === false ? $default : (string) $value;
};

return [
    // YesLogs sends: Authorization: Bearer <this value>
    'api_key' => $env('YESLOGS_API_KEY'),

    'database' => [
        'dsn' => $env(
            'CRM_DB_DSN',
            'mysql:host=127.0.0.1;port=3306;dbname=radius;charset=utf8mb4'
        ),
        'user' => $env('CRM_DB_USER', 'yeslogs_reader'),
        'password' => $env('CRM_DB_PASSWORD'),

        /*
         * Timezone used by radacct DATETIME columns.
         * Use UTC when radacct stores UTC, or Asia/Kolkata when it stores IST.
         */
        'radacct_timezone' => $env('CRM_RADACCT_TIMEZONE', 'UTC'),
    ],

    /*
     * Standard FreeRADIUS table and the normalized subscriber view described
     * in schema-reference.sql. Qualified names such as radius.radacct work.
     */
    'radacct_table' => $env('CRM_RADACCT_TABLE', 'radacct'),
    'subscriber_view' => $env('CRM_SUBSCRIBER_VIEW', 'yeslogs_subscribers'),

    /*
     * required     = radacct.nasipaddress must equal nasIpAddress
     * when_present = compare NAS IP when YesLogs supplied one (recommended)
     * ignore       = match only local IP + event time
     */
    'nas_match_mode' => $env('CRM_NAS_MATCH_MODE', 'when_present'),

    'max_lookups' => 1000,
    'max_body_bytes' => 2 * 1024 * 1024,
];
