<?php
// Hands over one finished restore. The service checks it belongs to the
// account asking before a byte is sent.
require_once __DIR__ . '/proxy.php';
cprest_page('/download');
