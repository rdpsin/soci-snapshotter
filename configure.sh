print_help() {
    printf "
./configure.sh [ARGUMENTS]

Arguments:

    -h, --help         Print usage
    --install-zlib     Installs zlib development package
    --dev              Installs ancillary tools required for development
    \n"
}


ZLIB_VERSION="1.2.13"
FLATC_VERSION="2.0.8"
TMPDIR=$(mktemp -d)

install_zlib() {
    printf "Installing zlib version: ${ZLIB_VERSION}\n" 
    ZLIB_DIR="zlib-${ZLIB_VERSION}"
    ZLIB_ARCHIVE="${ZLIB_DIR}.tar.gz"
    pushd ${TMPDIR}
    wget "https://www.zlib.net/fossils/${ZLIB_ARCHIVE}"
    tar xzvf ${ZLIB_ARCHIVE}
    cd $ZLIB_DIR; ./configure; sudo make install
    popd
}

install_cmake_no_check() {
    pushd ${TMPDIR}
    @wget https://github.com/Kitware/CMake/releases/download/v3.24.1/cmake-3.24.1-Linux-x86_64.sh -O cmake.sh
    @sh cmake.sh --prefix=/usr/local/ --exclude-subdir
    @rm -rf cmake.sh
    popd
}

install_cmake() {
    printf "Checking if CMake already exists\n"

    if ! command -v cmake &> /dev/null
    then
        printf "CMake does not exist. Installing it\n"
    else
        printf "CMake already exists. Will skip installation...\n"
        return 
    fi

    install_cmake_no_check
}

install_flatc_no_check() {
    printf "Installing flatc version: ${FLATC_VERSION}\n"
    pushd ${TMPDIR}
    wget https://github.com/google/flatbuffers/archive/refs/tags/${FLATC_VERSION}.tar.gz -O flatbuffers.tar.gz
    tar xzvf flatbuffers.tar.gz
    cd flatbuffers-${FLATC_VERSION} && cmake -G "Unix Makefiles" -DCMAKE_BUILD_TYPE=Release && make && make install
    popd
}

install_flatc() {
    printf "Checking if flatc already exists\n"
    if ! command -v flatc &> /dev/null
    then
        printf "flatc does not exist. Installing it\n"
    else
        printf "flatc already exists. Will skip installation...\n"
        return 
    fi
    
    install_flatc_no_check
}

install_check_tools() {
    pushd ${TMPDIR}
    @curl -sfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(shell go env GOPATH)/bin v1.49.0
	go install github.com/kunalkushwaha/ltag@v0.2.3
	go install github.com/vbatts/git-validation@v1.1.0
    popd
}

INSTALL_ZLIB=
INSTALL_DEV=


for i in "$@"; do
    case $i in
         -h|--help)
            print_help
            exit 2
            ;;
        --install-zlib)
            INSTALL_ZLIB=YES
            shift
            ;;
        --dev)
            printf INSTALL_DEV=YES
            shift
            ;;
    esac
done

if [[ "${INSTALL_DEV}" == "YES" ]]; then
    install_flatc
    install_check_tools
fi

if [[ "${INSTALL_ZLIB}" == "YES" ]]; then
    install_zlib
fi

