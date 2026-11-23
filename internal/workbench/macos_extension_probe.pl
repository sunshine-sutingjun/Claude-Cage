use strict;
use warnings;
use Errno qw(EACCES EPERM EROFS);
use File::Temp qw(tempdir);
use POSIX qw(_exit WNOHANG);
use Socket qw(AF_UNIX SOCK_STREAM PF_UNSPEC);
use Time::HiRes qw(clock_gettime CLOCK_MONOTONIC);

my $server_readonly = @ARGV == 1 && $ARGV[0] eq 'server-readonly';
die "invalid probe mode" if @ARGV && !$server_readonly;

# Two child processes connected only to one another. A local, read-only SFTP
# endpoint substitutes for the project transport and no host files are shared.
umask 077;
my $tmp = tempdir('cage-check-XXXXXXXX', DIR => '/private/var/tmp', CLEANUP => 0);
my $source = "$tmp/source";
my $mount = "$tmp/mount";
my @children;
my $attached = '';
sub mounted_line {
    open my $list, '-|', '/sbin/mount' or die "mount list";
    my @matches = grep { index($_, " on $mount (") >= 0 } <$list>;
    close $list or die "mount list";
    die "ambiguous mount" if @matches > 1;
    return $matches[0] // '';
}
sub spawn_peer {
    my ($side, $other, @cmd) = @_;
    my $pid = fork();
    die "fork" unless defined $pid;
    return $pid if $pid;
    close $other;
    open STDIN, '<&', $side or _exit(125);
    open STDOUT, '>&', $side or _exit(125);
    open STDERR, '>', '/dev/null' or _exit(125);
    close $side;
    exec @cmd or _exit(126);
}
my $ok = eval {
    local $SIG{TERM} = local $SIG{HUP} = local $SIG{INT} = sub { die "cancelled" };
    mkdir $source, 0700 or die "source";
    mkdir $mount, 0700 or die "target";
    open my $fixture, '>', "$source/readonly" or die "fixture";
    print {$fixture} 'cage-readonly-probe' or die "fixture";
    close $fixture or die "fixture";
    socketpair(my $a,my $b,AF_UNIX,SOCK_STREAM,PF_UNSPEC) or die "socketpair";
    push @children, spawn_peer($a,$b,'/usr/libexec/sftp-server','-R','-d',$source);
    # FSKit before macOS 26.4 cannot advertise a read-only mount. The SFTP
    # endpoint stays -R in both modes and remains the authority for writes.
    # This socketpair fixture lives entirely inside the guest. Mark it local
    # so testing it does not require access to network volumes over SSH.
    # Real host project mounts keep their separate folder authorization.
    my $options = 'slave,'.($server_readonly ? '' : 'ro,').'quiet,backend=fskit,local,nodev,nosuid,dir_cache=no';
    push @children, spawn_peer($b,$a,'/usr/local/libexec/cage-sshfs',":$source",$mount,'-f','-o',$options);
    close $a; close $b;
    my $mount_deadline = clock_gettime(CLOCK_MONOTONIC) + 12;
    while (1) {
        $attached = mounted_line();
        last if $attached;
        if (waitpid($children[1], WNOHANG) == $children[1]) {
            pop @children;
            die "SSHFS exited before mounting the probe volume";
        }
        last if clock_gettime(CLOCK_MONOTONIC) >= $mount_deadline;
        select undef,undef,undef,0.4;
    }
    die "probe volume did not mount" unless $attached;
    my $user = getpwuid($<);
    my ($flags) = $attached =~ m{^(?:macfuse://[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}|/dev/disk[0-9]+s[0-9]+) on \Q$mount\E \(macfuse, ([^\n]+)\)\s*$}i;
    my %options = map { $_ => 1 } split /, /, ($flags // '');
    # Some FSKit versions omit the read-only flag even when ro is requested.
    # Verify the effective policy through the mounted endpoint below.
    die "unexpected mount" unless $options{'fskit'} && $options{'local'} && $options{'nodev'} && $options{'nosuid'} && $options{"mounted by $user"};
    open my $read, '<', "$mount/readonly" or die "read-only mount unreadable";
    my $content = do { local $/; <$read> };
    close $read or die "read-only mount unreadable";
    die "wrong read-only source" unless $content eq 'cage-readonly-probe';
    if (open my $write, '>', "$mount/forbidden") {
        close $write;
        die "read-only endpoint accepted a write";
    }
    die "unexpected write failure: $!" unless $! == EACCES || $! == EPERM || $! == EROFS;
    die "read-only source was modified" if -e "$source/forbidden";
    1;
};
warn $@ unless $ok;
# Never unmount a replacement or remove a mounted tree.
my $live = mounted_line();
if ($live && $live eq $attached) {
    # The guest account may be denied an explicit unmount. FSKit can also
    # release the volume asynchronously when its SSHFS connection closes.
    system('/sbin/umount','-f',$mount);
}
for my $pid (@children) {
    if (waitpid($pid,WNOHANG)==0) {kill 'TERM',$pid}
}
select undef,undef,undef,0.15;
for my $pid (@children) {
    if (waitpid($pid,WNOHANG)==0) {kill 'KILL',$pid;waitpid($pid,0)}
}
for (1..20) {
    $live = mounted_line();
    last if !$live || $live ne $attached;
    select undef,undef,undef,0.1;
}
if (mounted_line()) {
    warn "probe mount still attached\n";
    $ok = 0;
} else {
    $ok = 0 unless rmdir $mount;
}
unlink "$source/forbidden" if -e "$source/forbidden";
$ok = 0 unless unlink "$source/readonly";
$ok = 0 unless rmdir $source;
$ok = 0 unless rmdir $tmp;
exit($ok ? 0 : 1);
