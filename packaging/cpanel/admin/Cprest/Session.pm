package Cpanel::Admin::Modules::Cprest::Session;

use strict;
use warnings;

use parent 'Cpanel::Admin::Base';

use IO::Socket::UNIX ();
use Socket qw(SOCK_STREAM);

use constant _actions => ('ISSUE_CAPABILITY');
use constant ADMIN_SOCKET => '/var/run/cprest/admin/ui.sock';

sub ISSUE_CAPABILITY {
    my ( $self, $method, $target ) = @_;

    return _denied() if !defined($method) || $method !~ /\A(?:GET|POST)\z/;
    return _denied() if !defined($target) || !length($target) || length($target) > 4096;
    return _denied() if $target =~ /[\x00-\x1f\x7f]/;

    $self->cpuser_has_feature_or_die('cprest');

    my $account   = $self->get_caller_username();
    my $principal = $account;
    my $team_user = $self->get_caller_team_user();
    if ( defined($team_user) && length($team_user) ) {
        my $domain = $self->get_caller_team_user_login_domain();
        return _denied() if !defined($domain) || !length($domain);
        $principal = $team_user . '@' . $domain;
    }

    return _denied() if $account !~ /\A[A-Za-z0-9_]{1,64}\z/;
    return _denied() if $principal !~ /\A[!-~]{1,256}\z/;

    require Cpanel::Session::Restore;
    my ( $session_name, $session );
    local $@;
    my $restored = eval {
        ( $session_name, $session ) = Cpanel::Session::Restore::restoreSession($principal);
        1;
    };
    return _denied() if !$restored || !defined($session_name) || ref($session) ne 'HASH';

    my $security_token = $session->{'cp_security_token'} // '';
    return _denied() if $session_name !~ /\A[A-Za-z0-9_:\@.+%\-!#\$=\?\^\{\}~]{12,256}\z/;
    return _denied() if $security_token !~ m{\A/cpsess[0-9]{1,20}\z};

    return _request_capability(
        $account, $principal, $session_name, $security_token,
        $method, $target,
    );
}

sub _request_capability {
    my ( $account, $principal, $session_name, $security_token, $method, $target ) = @_;

    my $socket = IO::Socket::UNIX->new(
        Type    => SOCK_STREAM,
        Peer    => ADMIN_SOCKET,
        Timeout => 5,
    );
    return { status => 503, token => '' } if !$socket;
    $socket->autoflush(1);

    my $request = "POST /_cprest/user-capability HTTP/1.1\r\n"
      . "Host: cprest\r\n"
      . "Connection: close\r\n"
      . "Content-Length: 0\r\n"
      . "X-Cprest-Cpanel-Account: $account\r\n"
      . "X-Cprest-Cpanel-Principal: $principal\r\n"
      . "X-Cprest-Cpanel-Session: $session_name\r\n"
      . "X-Cprest-Cpanel-Token: $security_token\r\n"
      . "X-Cprest-Request-Method: $method\r\n"
      . "X-Cprest-Request-Target: $target\r\n\r\n";

    if ( !print {$socket} $request ) {
        close $socket;
        return { status => 503, token => '' };
    }

    my $status_line = <$socket> // '';
    my ($status) = $status_line =~ m{\AHTTP/1\.[01] ([0-9]{3})};
    while ( my $line = <$socket> ) {
        last if $line eq "\r\n" || $line eq "\n";
    }
    local $/;
    my $token = <$socket> // '';
    close $socket;
    $token =~ s/\A\s+|\s+\z//g;

    $status = 502 if !defined($status);
    if ( $status == 200 && length($token) <= 4096 && $token =~ /\Av1\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\z/ ) {
        return { status => 200, token => $token };
    }
    return { status => ( $status == 503 ? 503 : 403 ), token => '' };
}

sub _denied {
    return { status => 403, token => '' };
}

1;
