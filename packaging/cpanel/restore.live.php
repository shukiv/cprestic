<?php
// Queues a restore, then sends the browser back to the list. No page of
// its own: what it produces appears where everything else does.
require_once __DIR__ . '/proxy.php';
cprest_page('/restore');
