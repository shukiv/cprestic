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

const CPREST_SOCKET = '/var/run/cprest/user.sock';

function cprest_page(string $path): void {
    $query = $_SERVER['QUERY_STRING'] ?? '';
    $target = $path . ($query === '' ? '' : '?' . $query);
    $method = ($_SERVER['REQUEST_METHOD'] ?? 'GET') === 'POST' ? 'POST' : 'GET';

    $body = '';
    if ($method === 'POST') {
        $body = file_get_contents('php://input');
        if ($body === false) { $body = ''; }
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
        . "Connection: close\r\n";
    if ($method === 'POST') {
        $request .= "Content-Type: application/x-www-form-urlencoded\r\n"
            . 'Content-Length: ' . strlen($body) . "\r\n";
    }
    $request .= "\r\n" . $body;

    fwrite($socket, $request);
    $response = stream_get_contents($socket);
    fclose($socket);

    $split = strpos($response, "\r\n\r\n");
    if ($split === false) {
        http_response_code(502);
        echo '<p>cP:Restic answered something this page could not read.</p>';
        return;
    }
    $head = substr($response, 0, $split);
    $payload = substr($response, $split + 4);

    // A redirect and a download have to reach the browser as themselves.
    foreach (explode("\r\n", $head) as $index => $line) {
        if ($index === 0) {
            if (preg_match('#^HTTP/1\.[01] (\d{3})#', $line, $status)) {
                http_response_code((int) $status[1]);
            }
            continue;
        }
        [$name, $value] = array_pad(explode(':', $line, 2), 2, '');
        $name = strtolower(trim($name));
        if (in_array($name, ['location', 'content-disposition', 'content-type', 'content-length'], true)) {
            header(trim($line));
        }
    }
    echo $payload;
}
