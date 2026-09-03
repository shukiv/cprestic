package Cpanel::API::Cprest;

use strict;
use warnings;

use Cpanel::AdminBin::Call ();

our %API = (
    issue_capability => {},
);

sub issue_capability {
    my ( $args, $result ) = @_;

    my $method = $args->get_length_required('method');
    my $target = $args->get_length_required('target');
    my $answer;

    local $@;
    my $ok = eval {
        $answer = Cpanel::AdminBin::Call::call(
            'Cprest', 'Session', 'ISSUE_CAPABILITY', $method, $target,
        );
        1;
    };

    if ( !$ok || ref($answer) ne 'HASH' ) {
        $answer = { status => 503, token => '' };
    }
    $result->data($answer);
    return 1;
}

1;
