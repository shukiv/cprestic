<?php
/**
 * cP:Restic — the account-facing plugin.
 *
 * It renders nothing itself. Every request is passed to the cP:Restic
 * service over a unix socket, and the answer is passed back.
 *
 * There is no account parameter anywhere in here on purpose. cPanel runs
 * this as the account it belongs to, so the service reads who is asking
 * from the socket itself (SO_PEERCRED). A customer cannot ask for another
 * customer's backups by editing a URL, because the URL never says whose
 * they are.
 */

const CPREST_SOCKET = '/var/run/cprest/account/user.sock';
const CPREST_MAX_BODY = 1048576;

require_once '/usr/local/cpanel/php/cpanel.php';

/**
 * Keep one LiveAPI connection for the page. cPanel explicitly requires a
 * .live.php application to instantiate this object only once.
 */
function cprest_cpanel(): CPANEL {
    static $cpanel = null;
    if ($cpanel === null) {
        $cpanel = new CPANEL();
    }
    return $cpanel;
}

function cprest_feature_enabled(): bool {
    static $enabled = null;
    if ($enabled === null) {
        $enabled = (bool) cprest_cpanel()->cpanelfeature('cprest');
    }
    return $enabled;
}

function cprest_write_all($socket, string $request): bool {
    $written = 0;
    $length = strlen($request);
    while ($written < $length) {
        $count = fwrite($socket, substr($request, $written));
        if ($count === false || $count === 0) {
            return false;
        }
        $written += $count;
    }
    return true;
}

/**
 * Ask cPanel's authenticated LiveAPI engine to exchange its root-owned
 * session record for a one-use token bound to this exact request. cPanel
 * deliberately withholds browser cookies from LivePHP, so the exchange goes
 * through the installed UAPI/AdminBin bridge rather than reading $_COOKIE.
 */
function cprest_capability(string $method, string $target): array {
    if (strlen($target) === 0 || strlen($target) > 4096 ||
        preg_match('/[\x00-\x1F\x7F]/', $target) ||
        !in_array($method, ['GET', 'POST'], true)) {
        return ['status' => 403, 'token' => ''];
    }

    try {
        $response = cprest_cpanel()->uapi('Cprest', 'issue_capability', [
            'method' => $method,
            'target' => $target,
        ]);
    } catch (Throwable $error) {
        return ['status' => 503, 'token' => ''];
    }

    $result = $response['cpanelresult']['result'] ?? null;
    $data = is_array($result) ? ($result['data'] ?? null) : null;
    $status = is_array($data) ? (int) ($data['status'] ?? 503) : 503;
    $token = is_array($data) && is_string($data['token'] ?? null) ? trim($data['token']) : '';
    if ($status !== 200 || strlen($token) > 4096 ||
        !preg_match('/^v1\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$/D', $token)) {
        return ['status' => $status, 'token' => ''];
    }
    return ['status' => 200, 'token' => $token];
}

function cprest_page(string $path): void {
    // Feature Manager must be an authorization boundary, not just a hidden
    // tile. Without this check an account denied the feature can type the
    // .live.php URL directly and still ask the root service for restores.
    if (!cprest_feature_enabled()) {
        http_response_code(403);
        header('Cache-Control: no-store, max-age=0');
        echo '<p>cP:Restic is not enabled for this account.</p>';
        return;
    }

    $query = $_SERVER['QUERY_STRING'] ?? '';
    $target = $path . ($query === '' ? '' : '?' . $query);
    $method = ($_SERVER['REQUEST_METHOD'] ?? 'GET') === 'POST' ? 'POST' : 'GET';

    $capability = cprest_capability($method, $target);
    if ($capability['status'] !== 200) {
        $status = $capability['status'] === 503 ? 503 : 403;
        http_response_code($status);
        header('Cache-Control: no-store, max-age=0');
        if ($status === 503) {
            echo '<p>cP:Restic could not verify your cPanel session. Ask your host to check the service.</p>';
        } else {
            echo '<p>Open cP:Restic from a current cPanel session as the account owner or an Administrator team user.</p>';
        }
        return;
    }

    $body = '';
    if ($method === 'POST') {
        $declared = $_SERVER['CONTENT_LENGTH'] ?? '0';
        if (!is_string($declared) || !ctype_digit($declared) || (int) $declared > CPREST_MAX_BODY) {
            http_response_code(413);
            header('Cache-Control: no-store, max-age=0');
            echo '<p>The submitted form is too large.</p>';
            return;
        }
        $body = file_get_contents('php://input');
        if ($body === false) { $body = ''; }
        if (strlen($body) > CPREST_MAX_BODY) {
            http_response_code(413);
            header('Cache-Control: no-store, max-age=0');
            echo '<p>The submitted form is too large.</p>';
            return;
        }
    }

    $socket = @stream_socket_client('unix://' . CPREST_SOCKET, $errno, $errstr, 15);
    if ($socket === false) {
        http_response_code(503);
        echo '<p>cP:Restic is not running on this server. Ask your host to start it.</p>';
        return;
    }
    stream_set_timeout($socket, 300);

    $request = $method . ' ' . $target . " HTTP/1.1\r\n"
        . "Host: cprest\r\n"
        . "Connection: close\r\n"
        . "X-Cprest-Capability: " . $capability['token'] . "\r\n";
    if ($method === 'POST') {
        $request .= "Content-Type: application/x-www-form-urlencoded\r\n"
            . 'Content-Length: ' . strlen($body) . "\r\n";
    }
    $request .= "\r\n" . $body;

    if (!cprest_write_all($socket, $request)) {
        fclose($socket);
        http_response_code(502);
        echo '<p>cP:Restic could not receive this request.</p>';
        return;
    }

    // Headers a line at a time, then the body in chunks. A restore
    // archive is gigabytes; reading the whole answer into a string first
    // would exhaust PHP's memory limit and hand the customer a blank
    // page instead of their files.
    $head = [];
    while (($line = fgets($socket)) !== false) {
        $line = rtrim($line, "\r\n");
        if ($line === '') { break; }
        $head[] = $line;
    }
    if ($head === []) {
        fclose($socket);
        http_response_code(502);
        echo '<p>cP:Restic answered something this page could not read.</p>';
        return;
    }

    if (preg_match('#^HTTP/1\.[01] (\d{3})#', $head[0], $status)) {
        http_response_code((int) $status[1]);
    }
    // A redirect and a download have to reach the browser as themselves.
    foreach (array_slice($head, 1) as $line) {
        $name = strtolower(trim(explode(':', $line, 2)[0] ?? ''));
        // cache-control travels too: these pages carry the account's own
        // backups, and a proxy or a shared browser keeping a copy of one
        // outlives the session that was allowed to see it.
        if (in_array($name, ['location', 'content-disposition', 'content-type',
                             'content-length', 'cache-control', 'pragma'], true)) {
            header($line);
        }
    }

    while (!feof($socket)) {
        $chunk = fread($socket, 65536);
        if ($chunk === false || $chunk === '') { break; }
        echo $chunk;
        flush();
    }
    fclose($socket);
}
