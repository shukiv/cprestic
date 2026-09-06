#!/usr/local/cpanel/3rdparty/bin/perl
#WHMADDON:Gniza:Gniza Backups
#ACLS:all
#
# The Gniza WHM plugin.
#
# It renders nothing itself. WHM's own header and footer go around a body
# fragment fetched from the Gniza service, which listens on a unix socket
# rather than a port: cPanel servers are multi-tenant, the untrusted users
# are already on the box, and this interface can read every stored
# credential.
#
# Perl rather than Go, because the chrome — the sidebar, the masthead, the
# session token — comes from Whostmgr::HTMLInterface, and only a Perl
# process can call it. Everything below is plumbing around that.

use strict;
use warnings;

use IO::Socket::UNIX ();
use Whostmgr::ACLS   ();
use Whostmgr::HTMLInterface ();

# A compile-time constant, so nothing writable can redirect the plugin.
my $SOCKET = '/var/run/gniza/admin/ui.sock';
my $TIMEOUT = 300;
my $MAX_BODY = 1024 * 1024;

run() unless caller();

sub run {
    Whostmgr::ACLS::init_acls();

    # Gniza can read and delete every backup on this server, so it is
    # root's tool only. The AppConfig registration also restricts it, but
    # this is the check enforced here, on every request.
    if ( !Whostmgr::ACLS::hasroot() ) {
        deny('Gniza is available to the root WHM account only.');
        return;
    }

    # The Go service applies the same limit, but the CGI used to allocate
    # the entire body before the service could reject it. Bound it here too
    # so a request cannot consume cpsrvd's Perl worker memory first.
    if ( ( $ENV{'REQUEST_METHOD'} || 'GET' ) eq 'POST' ) {
        my $length = $ENV{'CONTENT_LENGTH'} // '0';
        if ( $length !~ /\A\d+\z/ || $length > $MAX_BODY ) {
            print "Status: 413 Payload Too Large\r\n"
              . "Content-Type: text/plain; charset=utf-8\r\n"
              . "Cache-Control: no-store, max-age=0\r\n\r\n"
              . "The submitted form is too large.\n";
            return;
        }
    }

    my ( $status, $headers, $body, $socket ) = request();

    if ( !defined $status ) {
        chrome( 'The Gniza service is not running on this server. '
              . 'Start it with: <code>systemctl start gniza</code>' );
        return;
    }

    # A redirect and a file download both have to reach the browser
    # untouched: one carries no body, the other is not HTML.
    if ( $status =~ /^3/ || $headers->{'content-disposition'} ) {
        print "Status: $status\r\n";
        for my $name ( sort keys %$headers ) {
            print ucfirst($name) . ": " . $headers->{$name} . "\r\n";
        }
        print "\r\n";
        binmode STDOUT;
        # An archive is gigabytes. It goes out as it arrives rather than
        # being held in this process first, which used to be a way to run
        # cpsrvd out of memory by asking for a large restore.
        print $body if length $body;
        stream( $socket, \*STDOUT );
        close $socket;
        return;
    }

    my $rest;
    {
        local $/;
        $rest = <$socket>;
    }
    close $socket;
    chrome( $body . ( defined $rest ? $rest : '' ) );
}

# request forwards this CGI invocation to the service and returns its
# status, headers and body.
sub request {
    # SOCK_STREAM is the default for a unix socket, and naming it would
    # mean pulling in Socket just for the constant.
    my $socket = IO::Socket::UNIX->new(
        Peer    => $SOCKET,
        Timeout => 10,
    ) or return ( undef, undef, undef );

    my $method = $ENV{'REQUEST_METHOD'} || 'GET';
    my $query  = $ENV{'QUERY_STRING'}   || '';
    my $path   = '/' . ( $query ? "?$query" : '' );

    my $payload = '';
    if ( $method eq 'POST' ) {
        my $length = $ENV{'CONTENT_LENGTH'} || 0;
        while ( length($payload) < $length ) {
            my $read = read( STDIN, my $chunk, $length - length($payload) );
            last if !defined($read) || $read == 0;
            $payload .= $chunk;
        }
    }

    my $request = "$method $path HTTP/1.0\r\n"
      . "Host: Gniza\r\n"
      . "Connection: close\r\n";
    if ( $method eq 'POST' ) {
        $request .= "Content-Type: " . ( $ENV{'CONTENT_TYPE'} || 'application/x-www-form-urlencoded' ) . "\r\n";
        $request .= "Content-Length: " . length($payload) . "\r\n";
    }
    $request .= "\r\n" . $payload;

    $socket->autoflush(1);
    binmode $socket;
    print {$socket} $request;

    # Headers first, a line at a time, so a large body never has to be
    # in memory to find where it starts.
    my $head = '';
    my $body = '';
    while ( defined( my $chunk = <$socket> ) ) {
        $head .= $chunk;
        last if $chunk =~ /^\r?\n$/;
    }
    return ( undef, undef, undef ) unless length $head;
    $head =~ s/\r?\n\r?\n\z//;

    my @lines  = split /\r\n/, $head;
    my $line   = shift @lines || '';
    my $status = $line =~ m{^HTTP/\d\.\d\s+(\d{3}.*)$} ? $1 : '200 OK';

    my %headers;
    for my $header (@lines) {
        my ( $name, $value ) = split /:\s*/, $header, 2;
        next unless defined $value;
        $headers{ lc $name } = $value;
    }
    return ( $status, \%headers, $body, $socket );
}

# stream copies what is left of the socket to the browser in pieces.
sub stream {
    my ( $from, $to ) = @_;
    my $buffer;
    while ( my $read = read( $from, $buffer, 65536 ) ) {
        print {$to} $buffer;
    }
    return;
}

# chrome prints the fragment inside WHM's own interface.
sub chrome {
    my ($fragment) = @_;
    # No copy of a page that shows a repository password or a private key
    # should outlive the session that was allowed to see it.
    print "Content-type: text/html\r\nCache-Control: no-store, max-age=0\r\nPragma: no-cache\r\n\r\n";
    Whostmgr::HTMLInterface::defheader( 'Gniza Backups', '', '/cgi/gniza.cgi' );
    print $fragment;
    Whostmgr::HTMLInterface::deffooter();
}

sub deny {
    my ($message) = @_;
    print "Content-type: text/html\r\nStatus: 403 Forbidden\r\nCache-Control: no-store, max-age=0\r\n\r\n";
    print '<div style="font:14px system-ui,sans-serif;padding:2rem"><h1>Gniza backups</h1><p>'
      . $message . '</p></div>';
}
