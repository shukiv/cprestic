<?php
require_once __DIR__ . '/proxy.php';
$cpanel = cprest_cpanel();
if (!cprest_feature_enabled()) {
    http_response_code(403);
    print $cpanel->header('cP:Restic');
    print '<p>cP:Restic is not enabled for this account.</p>';
    print $cpanel->footer();
    exit;
}
print $cpanel->header('cP:Restic');
cprest_page('/browse');
print $cpanel->footer();
