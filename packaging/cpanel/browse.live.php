<?php
require_once __DIR__ . '/proxy.php';
require_once '/usr/local/cpanel/php/cpanel.php';
$cpanel = new CPANEL();
print $cpanel->header('cP:Restic');
cprest_page('/browse');
print $cpanel->footer();
