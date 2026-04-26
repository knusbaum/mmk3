main.o {
    cc -c main.c -o main.o
}

main.o : main.c lib.h {
    cc -c main.c -o main.o
}

file main.o : main.c lib.h {
    cc -c main.c -o main.o
}

file main.o on ubuntu : main.c lib.h {
    cc -c main.c -o main.o
}

# pseudo-grammar:
<type> <target> on <runner> : <deps, ...>  {
    <body>
}


# expansions in a definition:
OBJECTS=main \
    lib \
    somethingelse

for o in $OBJECTS; do
    file ${o}.o on ubuntu : ${o}.c {
	cc -c ${o}.c -o ${o}.o
    }
done

